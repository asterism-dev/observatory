package api

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

var builderRef = "ghcr.io/asterism-dev/observatory-builder@" + digestN(0xb0)

type buildEnv struct {
	*env
	sched     *builds.Scheduler
	agent     http.Handler
	manifests *fakeManifests
}

// withBuilds wires a scheduler whose nodes are all online, and the agent and registry endpoints.
func (e *env) withBuilds() *buildEnv { return e.withBuildLogs(nil) }

func (e *env) withBuildLogs(logs *telemetry.LogWriter) *buildEnv {
	e.t.Helper()
	m := e.withManifests()
	m.manifests = map[string]registry.Manifest{}
	be := &buildEnv{env: e, manifests: m}
	sched := builds.New(e.store, "http://hub.internal:8090/", builderRef, nil, e.api.fleet.NotifyOrg, func(uuid.UUID) bool { return true })
	e.api.builds = sched
	iss, err := registry.LoadIssuer(e.t.TempDir())
	if err != nil {
		e.t.Fatal(err)
	}
	mux := http.NewServeMux()
	BuildEndpoints(mux, e.store, sched, m, e.api.tags, logs)
	RegistryEndpoints(mux, e.store, e.z, Registry{Upstream: &url.URL{Scheme: "http", Host: "registry.invalid"}, PublicURL: &url.URL{Scheme: "https", Host: "hub.test"},
		Issuer: iss, Builds: e.store, Manifests: m, Tags: e.api.tags, EventsSecret: eventsSecret})
	be.sched, be.agent = sched, mux
	return be
}

func (e *buildEnv) node(org uuid.UUID) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	members, err := e.store.Members(ctx, org)
	if err != nil {
		e.t.Fatal(err)
	}
	n, tok, err := e.store.CreateNode(ctx, store.Node{OrgID: org, Name: "n" + uuid.NewString()[:8], CreatedBy: members[0].User.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.store.EnrollNode(ctx, tok); err != nil {
		e.t.Fatal(err)
	}
	e.exec("UPDATE nodes SET cpus = 8, memory_mb = 16384 WHERE id = $1", n.ID)
	return n.ID
}

// recipeImage creates an org image whose recipe references refs.
func (e *buildEnv) recipeImage(tok string, org uuid.UUID, name, refs string) uuid.UUID {
	e.t.Helper()
	id := e.createImage(tok, org, `{"name":"`+name+`","scope":"org"}`)
	if rec := e.putRecipe(tok, org, id, recipeTar(e.t, dockerfile), refs); rec.Code != 200 {
		e.t.Fatalf("recipe: %d %s", rec.Code, rec.Body.String())
	}
	return id
}

func buildsPath(org, img uuid.UUID) string { return imagePath(org, img) + "/builds" }

func (e *buildEnv) startBuild(tok string, org, img uuid.UUID) uuid.UUID {
	e.t.Helper()
	rec := e.do("POST", buildsPath(org, img), tok)
	if rec.Code != 202 {
		e.t.Fatalf("start build: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ BuildID uuid.UUID }
	decode(e.t, rec, &got)
	return got.BuildID
}

func (e *buildEnv) tick() {
	e.t.Helper()
	if err := e.sched.Tick(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *buildEnv) build(id uuid.UUID) store.ImageBuild {
	e.t.Helper()
	b, err := e.store.ImageBuild(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// running starts a build of img on a fresh node and returns it with its build secret.
func (e *buildEnv) running(tok string, org, img uuid.UUID) (store.ImageBuild, string) {
	e.t.Helper()
	e.node(org)
	id := e.startBuild(tok, org, img)
	e.tick()
	b := e.build(id)
	if b.State != store.BuildRunning || b.VMID == nil {
		e.t.Fatalf("build not running: %+v", b)
	}
	secret, err := e.store.BuildSecret(context.Background(), org, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return b, secret
}

func (e *buildEnv) agentDo(method, path, secret, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	e.agent.ServeHTTP(rec, req)
	return rec
}

func TestBuildRequiresRefUse(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.addMember(org, carol, store.RoleMember)
	e.createEnv(aliceTok, org, `{"name":"ORG_TOKEN","value":"v","kind":"secret","scope":"org"}`)
	img := e.recipeImage(aliceTok, org, "tools", `[{"kind":"secret","scope":"org","name":"ORG_TOKEN"}]`)
	bare := e.createImage(aliceTok, org, `{"name":"bare","scope":"org"}`)
	deny := e.createRole(aliceTok, org, `{"name":"no secrets","rules":[{"effect":"deny","object":"org/env","action":"env:use"}]}`)
	e.bind(aliceTok, org, authz.UserSubject(bob.ID), deny)

	if rec := e.do("POST", buildsPath(org, bare), aliceTok); rec.Code != 422 {
		t.Fatalf("no recipe: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("POST", buildsPath(org, img), carolTok); rec.Code != 403 {
		t.Fatalf("member without image:build: %d", rec.Code)
	}
	if rec := e.do("POST", buildsPath(org, img), bobTok); rec.Code != 403 || !strings.Contains(rec.Body.String(), "forbidden") {
		t.Fatalf("builder without use on the reference: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("env:use"); len(es) != 1 || es[0].ActorID != bob.ID.String() || es[0].Result != store.ResultDenied {
		t.Fatalf("denied audit: %+v", es)
	}
	id := e.startBuild(aliceTok, org, img)
	if rec := e.do("POST", buildsPath(org, img), aliceTok); rec.Code != 409 {
		t.Fatalf("second build: %d", rec.Code)
	}
	if es := e.audit(store.ActionImageBuild); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Details["buildId"] != id.String() {
		t.Fatalf("build audit: %+v", es)
	}
	e.exec("DELETE FROM env_vars WHERE name = 'ORG_TOKEN'")
	if _, err := e.sched.Finish(context.Background(), id, store.BuildCancelled, "", ""); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("POST", buildsPath(org, img), aliceTok); rec.Code != 422 || !strings.Contains(rec.Body.String(), "no longer exists") {
		t.Fatalf("deleted reference: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBootEnvSkipsGcloud(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.connect(aliceTok, org, gcloudBody("user", adcJSON("authorized_user", dummyGRefres, "my-project"), ""))
	img := e.recipeImage(aliceTok, org, "tools", "")
	b, _ := e.running(aliceTok, org, img)
	// The API refuses such a recipe; a revision stored before that rule must still not hand out the token.
	e.exec(`UPDATE recipe_revisions SET refs = jsonb_build_array(jsonb_build_object('Kind', 'integration', 'Scope', 'user', 'OwnerID', $1::text, 'Name', 'gcloud')) WHERE image_id = $2`, alice.ID.String(), img)
	ctx := context.Background()
	v, err := e.store.VM(ctx, org, *b.VMID)
	if err != nil {
		t.Fatal(err)
	}
	env, tokens, err := e.sched.BootEnv(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	// Listed without a token, so the agent fails the build naming it.
	if len(tokens) != 0 || env["OBSERVATORY_BUILD_INTEGRATIONS"] != "gcloud" {
		t.Fatalf("gcloud handed to a build: %v %+v", env, tokens)
	}
	if igs := e.listIntegrations(aliceTok, org).Integrations; len(igs) != 1 || igs[0].WasInVM {
		t.Fatalf("gcloud marked as in a VM: %+v", igs)
	}
}

func TestBuildCreatesHiddenBuilderVM(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.createEnv(aliceTok, org, `{"name":"ORG_TOKEN","value":"real-value-123","kind":"secret","scope":"org","hosts":["api.example.com"]}`)
	e.createEnv(aliceTok, org, `{"name":"UNUSED","value":"x","kind":"secret","scope":"org"}`)
	e.createEnv(aliceTok, org, `{"name":"MINE","value":"y","kind":"secret","scope":"user"}`)
	e.connect(aliceTok, org, githubBody("org", ""))
	img := e.recipeImage(aliceTok, org, "tools", `[{"kind":"secret","scope":"org","name":"ORG_TOKEN"},{"kind":"integration","scope":"org","name":"github"}]`)
	b, secret := e.running(aliceTok, org, img)
	ctx := context.Background()

	var list struct{ VMs []vmJSON }
	decode(t, e.do("GET", "/api/orgs/"+org.String()+"/vms", aliceTok), &list)
	if len(list.VMs) != 0 {
		t.Fatalf("builder VM listed: %+v", list.VMs)
	}
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/vms/"+b.VMID.String(), aliceTok); rec.Code != 404 {
		t.Fatalf("builder VM by id: %d", rec.Code)
	}
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/builder-vms", bobTok); rec.Code != 403 {
		t.Fatalf("member sees builder VMs: %d", rec.Code)
	}
	decode(t, e.do("GET", "/api/orgs/"+org.String()+"/builder-vms", aliceTok), &list)
	if len(list.VMs) != 1 || list.VMs[0].ID != *b.VMID || list.VMs[0].CPUs != 2 || list.VMs[0].MemoryMB != 4096 || list.VMs[0].DiskGB != 40 ||
		list.VMs[0].Owner.ID != alice.ID || list.VMs[0].ImageRef != builderRef {
		t.Fatalf("admin view: %+v", list.VMs)
	}

	v, err := e.store.VM(ctx, org, *b.VMID)
	if err != nil {
		t.Fatal(err)
	}
	env, tokens, err := e.sched.BootEnv(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ORG_TOKEN":                      "real-value-123",
		"OBSERVATORY_BUILD_URL":          "http://hub.internal:8090",
		"OBSERVATORY_BUILD_ID":           b.ID.String(),
		"OBSERVATORY_BUILD_SECRET":       secret,
		"OBSERVATORY_BUILD_IMAGE":        img.String(),
		"OBSERVATORY_BUILD_SECRETS":      "ORG_TOKEN",
		"OBSERVATORY_BUILD_INTEGRATIONS": "github",
	}
	if !maps.Equal(env, want) {
		t.Fatalf("env = %v", env)
	}
	if len(tokens) != 1 || tokens[0].Kind != "github" || tokens[0].Token != dummyGitHub {
		t.Fatalf("integration tokens: %+v", tokens)
	}
	if igs := e.listIntegrations(aliceTok, org).Integrations; len(igs) != 1 || !igs[0].WasInVM {
		t.Fatalf("real token in a builder not recorded: %+v", igs)
	}

	device, _ := e.createDevice(aliceTok, org, "laptop")
	plain, err := e.store.CreateVM(ctx, store.VM{OrgID: org, NodeID: *b.NodeID, UserID: &alice.ID, Name: "plain", CPUs: 1, MemoryMB: 512, DiskGB: 10, CreatedBy: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.reload()
	st, err := e.net.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ports map[string][]string
	for _, o := range st.Orgs {
		if o.ID != org.String() {
			continue
		}
		ports = map[string][]string{}
		for _, p := range o.Ports {
			ports[p.Name] = p.AllowFrom
		}
		for name := range o.Records {
			if strings.HasPrefix(name, "build-") {
				t.Fatalf("builder DNS record %s", name)
			}
		}
		if o.Records["plain.internal"] == "" {
			t.Fatalf("records: %v", o.Records)
		}
	}
	if allow, ok := ports[b.VMID.String()+"-mgmt"]; !ok || len(allow) != 0 {
		t.Fatalf("builder mgmt port: present %v, allowFrom %v", ok, allow)
	}
	if allow := ports[plain.ID.String()+"-mgmt"]; !slices.Equal(allow, []string{device.IP}) {
		t.Fatalf("plain VM allowFrom %v, device %s", allow, device.IP)
	}
	if got, err := e.store.BuildBySecret(ctx, secret); err != nil || got.ID != b.ID {
		t.Fatalf("build by secret: %+v %v", got, err)
	}
	var hash []byte
	if err := e.pool.QueryRow(ctx, "SELECT secret_hash FROM image_builds WHERE id = $1", b.ID).Scan(&hash); err != nil || len(hash) != 32 || strings.Contains(string(hash), secret) {
		t.Fatalf("stored secret: %x %v", hash, err)
	}
}

func TestBuildTimeoutDeletesVM(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.recipeImage(aliceTok, org, "tools", "")
	b, secret := e.running(aliceTok, org, img)
	ctx := context.Background()

	e.sched.Now = func() time.Time { return b.StartedAt.Add(59 * time.Minute) }
	e.tick()
	if got := e.build(b.ID); got.State != store.BuildRunning {
		t.Fatalf("ended early: %+v", got)
	}
	e.sched.Now = func() time.Time { return b.StartedAt.Add(builds.Timeout) }
	e.tick()
	got := e.build(b.ID)
	if got.State != store.BuildFailed || !strings.Contains(got.Error, "timed out") || got.FinishedAt == nil {
		t.Fatalf("after timeout: %+v", got)
	}
	if _, err := e.store.VM(ctx, org, *b.VMID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("builder VM kept: %v", err)
	}
	if _, _, finished, err := e.store.BuildLogSince(ctx, b.ID, 0); err != nil || !finished {
		t.Fatalf("log not finished: %v", err)
	}
	if _, err := e.store.BuildBySecret(ctx, secret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("secret valid after the build: %v", err)
	}
	if tags, _ := e.store.ImageTags(ctx, img); len(tags) != 0 {
		t.Fatalf("tags moved: %v", tags)
	}

	queued := e.startBuild(aliceTok, org, e.recipeImage(aliceTok, org, "other", ""))
	e.exec("UPDATE nodes SET memory_mb = 1024")
	e.sched.Now = time.Now
	e.tick()
	if got := e.build(queued); got.State != store.BuildQueued {
		t.Fatalf("placed without room: %+v", got)
	}
	e.sched.Now = func() time.Time { return time.Now().Add(builds.Timeout + time.Second) } // slack for the database clock
	e.tick()
	if got := e.build(queued); got.State != store.BuildFailed || !strings.Contains(got.Error, "no node") {
		t.Fatalf("queued timeout: %+v", got)
	}
}

func TestOrphanBuilderVMDeleted(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	v, err := e.store.CreateVM(context.Background(), store.VM{OrgID: org, NodeID: e.node(org), UserID: &alice.ID, Name: "build-orphan", Kind: store.VMKindBuilder,
		CPUs: 1, MemoryMB: 512, DiskGB: 10, CreatedBy: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.tick()
	if _, err := e.store.VM(context.Background(), org, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan builder VM kept: %v", err)
	}
}

func TestBuildFailsNamingTheLimit(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	thirty := 30
	if err := e.store.SetLimits(context.Background(), org, store.ScopeOrg, store.Limits{DiskGB: &thirty}); err != nil {
		t.Fatal(err)
	}
	e.node(org)
	id := e.startBuild(aliceTok, org, e.recipeImage(aliceTok, org, "tools", ""))
	e.tick()
	if b := e.build(id); b.State != store.BuildFailed || !strings.Contains(b.Error, "the org's disk limit of 30 GB would be exceeded (40 GB)") {
		t.Fatalf("build: %+v", b)
	}
}

func TestBuildLogFollowSSE(t *testing.T) {
	old := logPollInterval
	logPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.recipeImage(aliceTok, org, "tools", "")
	b, secret := e.running(aliceTok, org, img)
	if rec := e.agentDo("POST", "/api/builds/"+b.ID.String()+"/log", secret, "line one\r\n"); rec.Code != 204 {
		t.Fatalf("log: %d %s", rec.Code, rec.Body.String())
	}
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+buildsPath(org, img)+"/"+b.ID.String()+"/log?follow=1", nil)
	req.Header.Set("Authorization", "Bearer "+aliceTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("follow: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		for lines.Scan() {
			if l := lines.Text(); l != "" {
				return l
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return ""
	}
	if l := next(); l != "data: line one" {
		t.Fatalf("first line %q", l)
	}
	e.agentDo("POST", "/api/builds/"+b.ID.String()+"/log", secret, "line two\npart")
	if l := next(); l != "data: line two" {
		t.Fatalf("second line %q", l)
	}
	if _, err := e.sched.Finish(context.Background(), b.ID, store.BuildFailed, "", "boom"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"data: part", "event: end", "data: failed"} {
		if l := next(); l != want {
			t.Fatalf("got %q, want %q", l, want)
		}
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	rec := e.do("GET", buildsPath(org, img)+"/"+b.ID.String()+"/log", aliceTok)
	if rec.Code != 200 || rec.Body.String() != "line one\r\nline two\npart" {
		t.Fatalf("full log: %d %q", rec.Code, rec.Body.String())
	}
	var list []buildJSON
	decode(t, e.do("GET", buildsPath(org, img), aliceTok), &list)
	if len(list) != 1 || list[0].ID != b.ID || list[0].State != store.BuildFailed || list[0].Error != "boom" {
		t.Fatalf("builds: %+v", list)
	}
}

func TestAgentEndpointsNeedBuildSecret(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	team := e.createTeam(aliceTok, org, "core")
	img := e.recipeImage(aliceTok, org, "tools", "")
	base := e.createImage(aliceTok, org, `{"name":"base","scope":"org"}`)
	teamImg := e.createImage(aliceTok, org, `{"name":"t","scope":"team","teamId":"`+team.String()+`"}`)
	mine := e.createImage(aliceTok, org, `{"name":"mine","scope":"user"}`)
	b, secret := e.running(aliceTok, org, img)
	path := "/api/builds/" + b.ID.String()

	for _, c := range []struct{ name, path, secret string }{
		{"no secret", path, ""},
		{"wrong secret", path, "nope"},
		{"other build", "/api/builds/" + uuid.NewString(), secret},
	} {
		for _, ep := range []struct{ method, path string }{{"GET", "/context"}, {"POST", "/log"}, {"POST", "/result"}} {
			if rec := e.agentDo(ep.method, c.path+ep.path, c.secret, `{"error":"x"}`); rec.Code != 401 {
				t.Fatalf("%s %s: %d", c.name, ep.path, rec.Code)
			}
		}
	}
	rec := e.agentDo("GET", path+"/context", secret, "")
	if rec.Code != 200 {
		t.Fatalf("context: %d %s", rec.Code, rec.Body.String())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "Dockerfile" {
		t.Fatalf("context entry: %+v %v", hdr, err)
	}
	if content, _ := io.ReadAll(tr); string(content) != string(dockerfile.content) {
		t.Fatalf("Dockerfile: %q", content)
	}
	if rec := e.agentDo("POST", path+"/log", secret, "hello\n"); rec.Code != 204 {
		t.Fatalf("log: %d", rec.Code)
	}

	for _, c := range []struct {
		name    string
		id      uuid.UUID
		actions string
		want    []string
	}{
		{"own image", img, "pull,push", []string{"pull", "push"}},
		{"org base image", base, "pull,push", []string{"pull"}},
		{"team image below an org image", teamImg, "pull", nil},
		{"private image below an org image", mine, "pull", nil},
	} {
		code, got := grant(t, e.agent, "observatory", secret, repoScope(c.id, c.actions))
		if code != 200 || !slices.Equal(got["i/"+c.id.String()], c.want) {
			t.Fatalf("%s: %d %v", c.name, code, got)
		}
	}
	noUse := e.createRole(aliceTok, org, `{"name":"no images","rules":[{"effect":"deny","object":"org/images","action":"image:use"}]}`)
	e.bind(aliceTok, org, authz.UserSubject(alice.ID), noUse)
	if _, got := grant(t, e.agent, "observatory", secret, repoScope(base, "pull")); len(got) != 0 {
		t.Fatalf("base pull without the starter's image:use: %v", got)
	}

	rec = e.do("POST", buildsPath(org, img)+"/"+b.ID.String()+"/cancel", aliceTok)
	var cancelled buildJSON
	decode(t, rec, &cancelled)
	if rec.Code != 200 || cancelled.State != store.BuildCancelled || cancelled.Error != "cancelled by alice" {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("POST", buildsPath(org, img)+"/"+b.ID.String()+"/cancel", aliceTok); rec.Code != 409 {
		t.Fatalf("cancel twice: %d", rec.Code)
	}
	if rec := e.agentDo("GET", path+"/context", secret, ""); rec.Code != 401 {
		t.Fatalf("context after the build: %d", rec.Code)
	}
	if code, _ := grant(t, e.agent, "observatory", secret, repoScope(img, "pull")); code != 401 {
		t.Fatalf("registry token after the build: %d", code)
	}
}

func TestBuildResultLinksVersion(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.recipeImage(aliceTok, org, "tools", "")
	ctx := context.Background()
	layer := registry.Layer{Digest: digestN(0x1a), Size: 7}

	// The result arrives before the push event.
	b, secret := e.running(aliceTok, org, img)
	first := digestN(1)
	e.manifests.manifests[first] = amd64Manifest(layer)
	if rec := e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"digest":"`+first+`"}`); rec.Code != 204 {
		t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.build(b.ID); got.State != store.BuildSucceeded || got.Digest != first {
		t.Fatalf("build: %+v", got)
	}
	v, err := e.store.ImageVersion(ctx, img, first)
	if err != nil || v.Source != store.VersionFromBuild || v.BuildID == nil || *v.BuildID != b.ID || v.SizeBytes != 7 || *v.CreatedBy != alice.ID {
		t.Fatalf("version: %+v %v", v, err)
	}
	tags, _ := e.store.ImageTags(ctx, img)
	if tags["latest"] != first || tags["build-"+b.ID.String()] != first {
		t.Fatalf("tags: %v", tags)
	}
	if _, err := e.store.VM(ctx, org, *b.VMID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("builder VM kept: %v", err)
	}
	if code := pushEvent(t, e.agent, eventsSecret, imageRepo(img), first, "build-"+b.ID.String(), "build:"+b.ID.String()); code != 200 {
		t.Fatalf("late push event: %d", code)
	}

	// The push event arrives first.
	b2, secret2 := e.running(aliceTok, org, img)
	second := digestN(2)
	e.manifests.manifests[second] = amd64Manifest(layer)
	if code := pushEvent(t, e.agent, eventsSecret, imageRepo(img), second, "build-"+b2.ID.String(), "build:"+b2.ID.String()); code != 200 {
		t.Fatalf("push event: %d", code)
	}
	if tags, _ := e.store.ImageTags(ctx, img); tags["latest"] != first {
		t.Fatalf("push event moved latest: %v", tags)
	}
	if rec := e.agentDo("POST", "/api/builds/"+b2.ID.String()+"/result", secret2, `{"digest":"`+second+`"}`); rec.Code != 204 {
		t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
	}
	if tags, _ := e.store.ImageTags(ctx, img); tags["latest"] != second {
		t.Fatalf("latest after result: %v", tags)
	}
	e.api.tags.wait()
	if got := e.manifests.tag(imageRepo(img), "latest"); got != second {
		t.Fatalf("registry latest %s, want %s", got, second)
	}

	// Failures move nothing.
	b3, secret3 := e.running(aliceTok, org, img)
	if rec := e.agentDo("POST", "/api/builds/"+b3.ID.String()+"/result", secret3, `{"error":"RUN failed"}`); rec.Code != 204 {
		t.Fatalf("error result: %d", rec.Code)
	}
	if got := e.build(b3.ID); got.State != store.BuildFailed || got.Error != "RUN failed" {
		t.Fatalf("failed build: %+v", got)
	}
	b4, secret4 := e.running(aliceTok, org, img)
	arm := digestN(4)
	e.manifests.manifests[arm] = registry.Manifest{Platform: "linux/arm64", Layers: []registry.Layer{layer}, Blobs: []registry.Layer{layer}}
	if rec := e.agentDo("POST", "/api/builds/"+b4.ID.String()+"/result", secret4, `{"digest":"`+arm+`"}`); rec.Code != 204 {
		t.Fatalf("arm result: %d", rec.Code)
	}
	if got := e.build(b4.ID); got.State != store.BuildFailed || !strings.Contains(got.Error, "linux/amd64") {
		t.Fatalf("arm build: %+v", got)
	}
	if tags, _ := e.store.ImageTags(ctx, img); tags["latest"] != second {
		t.Fatalf("failed builds moved latest: %v", tags)
	}
	if rec := e.agentDo("POST", "/api/builds/"+b4.ID.String()+"/result", secret4, `{"error":"again"}`); rec.Code != 401 {
		t.Fatalf("result after the build: %d", rec.Code)
	}
}

func TestBuildRenamesOnVMNameConflict(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.recipeImage(aliceTok, org, "tools", "")
	node := e.node(org)
	id := e.startBuild(aliceTok, org, img)
	taken := "build-" + strings.ReplaceAll(id.String(), "-", "")
	if _, err := e.store.CreateVM(context.Background(), store.VM{OrgID: org, NodeID: node, UserID: &alice.ID, Name: taken, CPUs: 1, MemoryMB: 512, DiskGB: 10, CreatedBy: alice.ID}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	b := e.build(id)
	if b.State != store.BuildRunning || b.VMID == nil {
		t.Fatalf("build: %+v", b)
	}
	v, err := e.store.VM(context.Background(), org, *b.VMID)
	if err != nil || v.Kind != store.VMKindBuilder || !strings.HasPrefix(v.Name, taken+"-") || len(v.Name) != len(taken)+5 {
		t.Fatalf("builder VM: %+v %v", v, err)
	}
}

func TestQueuedBuildWaitsWhileHeld(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "tools", `[]`)
	held := true
	e.sched.MayStart = func() bool { return !held }
	id := e.startBuild(aliceTok, org, img)
	e.tick()
	if b := e.build(id); b.State != store.BuildQueued {
		t.Fatalf("held build state %s", b.State)
	}
	e.exec("UPDATE image_builds SET queued_at = now() - interval '2 hours' WHERE id = $1", id)
	e.tick()
	held = false
	e.tick()
	if b := e.build(id); b.State != store.BuildRunning {
		t.Fatalf("released build state %s", b.State)
	}
}

type fakeGitHub struct {
	sha     string
	status  int // answered to every request when set
	tarball []byte
	auth    []string
}

// withGitHub points the hub's GitHub fetches at a fake serving img/Dockerfile of o/r at sha.
func (e *buildEnv) withGitHub() *fakeGitHub {
	e.t.Helper()
	gh := &fakeGitHub{sha: testSHA, tarball: makeTarball(e.t, ghEntry{name: "o-r-x/img/Dockerfile", body: "FROM scratch\n", mode: 0o644})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.auth = append(gh.auth, r.Header.Get("Authorization"))
		switch {
		case gh.status != 0:
			w.WriteHeader(gh.status)
		case strings.Contains(r.URL.Path, "/commits/"):
			w.Write([]byte(gh.sha))
		default:
			w.Write(gh.tarball)
		}
	}))
	e.t.Cleanup(srv.Close)
	e.api.github = gitHubSource{API: srv.URL}
	return gh
}

func (e *buildEnv) sourcedImage(tok string, org uuid.UUID, body string) uuid.UUID {
	e.t.Helper()
	id := e.createImage(tok, org, body)
	if rec := e.send("PUT", imagePath(org, id)+"/source", tok, sourceBody); rec.Code != 200 {
		e.t.Fatalf("source: %d %s", rec.Code, rec.Body.String())
	}
	return id
}

func (e *buildEnv) cancel(id uuid.UUID) {
	e.t.Helper()
	if _, err := e.sched.Finish(context.Background(), id, store.BuildCancelled, "", ""); err != nil {
		e.t.Fatal(err)
	}
}

func (e *buildEnv) log(id uuid.UUID) string {
	e.t.Helper()
	raw, err := e.store.BuildLog(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(raw)
}

func TestBuildFetchesFirstRevision(t *testing.T) {
	e := newEnv(t).withBuilds()
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.withGitHub()
	img := e.sourcedImage(tok, org, `{"name":"base","scope":"org"}`)

	id := e.startBuild(tok, org, img)
	rev, err := e.store.RecipeRevision(context.Background(), img, 1)
	if err != nil || rev.Source != "github:o/r@"+testSHA+":img" || rev.CreatedBy != alice.ID || len(rev.Files) != 1 || rev.Files[0].Path != "Dockerfile" {
		t.Fatalf("revision: %+v %v", rev, err)
	}
	if b := e.build(id); b.Revision != 1 {
		t.Fatalf("build: %+v", b)
	}
	if log := e.log(id); log != "fetching o/r@main → 0123456\nrevision 1 from 0123456\n" {
		t.Fatalf("log: %q", log)
	}
}

func TestBuildReusesUnchangedCommit(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	gh := e.withGitHub()
	e.createEnv(tok, org, `{"name":"ORG_TOKEN","value":"v","kind":"secret","scope":"org"}`)
	img := e.sourcedImage(tok, org, `{"name":"base","scope":"org"}`)
	e.cancel(e.startBuild(tok, org, img))
	e.exec(`UPDATE recipe_revisions SET refs = '[{"Kind":"secret","Scope":"org","Name":"ORG_TOKEN"}]' WHERE image_id = $1`, img)

	id := e.startBuild(tok, org, img)
	if i := e.imageByID(img); *i.RecipeRevision != 1 || e.build(id).Revision != 1 {
		t.Fatalf("new revision for the same commit: %+v", i)
	}
	if log := e.log(id); !strings.Contains(log, "unchanged since revision 1\n") {
		t.Fatalf("log: %q", log)
	}
	e.cancel(id)

	gh.sha = strings.Repeat("b", 40)
	id = e.startBuild(tok, org, img)
	rev, err := e.store.RecipeRevision(context.Background(), img, 2)
	if err != nil || e.build(id).Revision != 2 || len(rev.Refs) != 1 || rev.Refs[0].Name != "ORG_TOKEN" || rev.Source != "github:o/r@"+gh.sha+":img" {
		t.Fatalf("revision 2: %+v %v", rev, err)
	}
	if log := e.log(id); !strings.Contains(log, "revision 2 from bbbbbbb\n") {
		t.Fatalf("log: %q", log)
	}
}

func TestBuildSourceFailures(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	gh := e.withGitHub()
	img := e.sourcedImage(tok, org, `{"name":"base","scope":"org"}`)

	gh.status = http.StatusNotFound
	rec := e.do("POST", buildsPath(org, img), tok)
	if body := rec.Body.String(); rec.Code != 422 || !strings.Contains(body, "not found or no access") || !strings.Contains(body, "connect a GitHub integration") {
		t.Fatalf("404: %d %s", rec.Code, body)
	}
	gh.status = http.StatusServiceUnavailable
	if rec := e.do("POST", buildsPath(org, img), tok); rec.Code != 502 {
		t.Fatalf("503: %d %s", rec.Code, rec.Body.String())
	}
	gh.status, gh.tarball = 0, makeTarball(t, ghEntry{name: "o-r-x/img/README", body: "hi", mode: 0o644})
	if rec := e.do("POST", buildsPath(org, img), tok); rec.Code != 422 || !strings.Contains(rec.Body.String(), "the recipe needs a Dockerfile at its root") {
		t.Fatalf("no Dockerfile: %d %s", rec.Code, rec.Body.String())
	}
	if bs, _ := e.store.ImageBuilds(context.Background(), img); len(bs) != 0 {
		t.Fatalf("builds: %+v", bs)
	}
	if i := e.imageByID(img); i.RecipeRevision != nil {
		t.Fatalf("revision: %v", *i.RecipeRevision)
	}
}

func TestBuildUsesNarrowestGitHubIntegration(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	gh := e.withGitHub()
	img := e.sourcedImage(tok, org, `{"name":"base","scope":"user"}`)

	e.cancel(e.startBuild(tok, org, img))
	if slices.ContainsFunc(gh.auth, func(a string) bool { return a != "" }) {
		t.Fatalf("anonymous fetch sent %q", gh.auth)
	}
	e.connect(tok, org, `{"kind":"github","source":"pat","scope":"org","token":"ghp_org000000000000000"}`)
	e.connect(tok, org, `{"kind":"github","source":"pat","scope":"user","token":"ghp_private0000000000"}`)
	gh.auth = nil
	e.startBuild(tok, org, img)
	if len(gh.auth) == 0 || slices.ContainsFunc(gh.auth, func(a string) bool { return a != "Bearer ghp_private0000000000" }) {
		t.Fatalf("auth: %q", gh.auth)
	}
}

func TestBuildJSONSource(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.withGitHub()
	img := e.sourcedImage(tok, org, `{"name":"base","scope":"org"}`)
	e.startBuild(tok, org, img)
	if rec := e.do("GET", buildsPath(org, img), tok); !strings.Contains(rec.Body.String(), `"source":"github:o/r@`+testSHA+`:img"`) {
		t.Fatalf("builds: %s", rec.Body.String())
	}
}
