package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

type autoEnv struct {
	*buildEnv
	auto  *AutoRebuilds
	now   time.Time
	bases map[string]string // what public refs resolve to; a missing ref fails
	looks int
}

func (e *buildEnv) withAuto() *autoEnv {
	a := &autoEnv{buildEnv: e, now: time.Now(), bases: map[string]string{}}
	a.auto = NewAutoRebuilds(e.store, e.z, e.api.iam, e.sched, "https://hub.test", "http://hub.internal:8090")
	a.auto.Now = func() time.Time { return a.now }
	a.auto.resolve = func(_ context.Context, ref name.Reference) (string, error) {
		a.looks++
		if d, ok := a.bases[ref.Name()]; ok {
			return d, nil
		}
		return "", errors.New("registry unavailable")
	}
	return a
}

func (a *autoEnv) autoTick() {
	a.t.Helper()
	if err := a.auto.Tick(context.Background()); err != nil {
		a.t.Fatal(err)
	}
}

func (a *autoEnv) setAuto(tok string, org, img uuid.UUID, body string) {
	a.t.Helper()
	if rec := a.send("PATCH", imagePath(org, img), tok, body); rec.Code != 200 {
		a.t.Fatalf("auto settings: %d %s", rec.Code, rec.Body.String())
	}
}

func (a *autoEnv) image(id uuid.UUID) store.Image {
	a.t.Helper()
	img, err := a.store.ImageByID(context.Background(), id)
	if err != nil {
		a.t.Fatal(err)
	}
	return img
}

// builds returns the image's builds, newest first.
func (a *autoEnv) builds(img uuid.UUID) []store.ImageBuild {
	a.t.Helper()
	bs, err := a.store.ImageBuilds(context.Background(), img)
	if err != nil {
		a.t.Fatal(err)
	}
	return bs
}

// finish runs the queued build of img and reports digest n with bases, or failure when it is not empty.
func (a *autoEnv) finish(org, img uuid.UUID, n int, bases, failure string) store.ImageBuild {
	a.t.Helper()
	a.tick()
	b := a.builds(img)[0]
	if b.State != store.BuildRunning {
		a.t.Fatalf("build not running: %+v", b)
	}
	secret, err := a.store.BuildSecret(context.Background(), org, b.ID)
	if err != nil {
		a.t.Fatal(err)
	}
	body := `{"error":"` + failure + `"}`
	if failure == "" {
		a.manifests.manifests[digestN(n)] = amd64Manifest(registry.Layer{Digest: digestN(0x1a), Size: 7})
		body = `{"digest":"` + digestN(n) + `","bases":` + bases + `}`
	}
	if rec := a.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, body); rec.Code != 204 {
		a.t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
	}
	return a.build(b.ID)
}

func (a *autoEnv) latest(img uuid.UUID) string {
	a.t.Helper()
	d, err := a.store.ResolveImageTag(context.Background(), img, "latest")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.t.Fatal(err)
	}
	return d
}

func TestAutoSettings(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	alice, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	img := e.recipeImage(aliceTok, org, "basis", "")
	bare := e.createImage(aliceTok, org, `{"name":"bare","scope":"org"}`)
	noBuild := e.createRole(org, `{"name":"no builds","rules":[{"effect":"deny","object":"org/images","action":"build.builds.create"}]}`)
	e.bind(org, authz.UserSubject(bob.ID), noBuild)

	if rec := e.send("PATCH", imagePath(org, img), aliceTok, `{"autoSchedule":"hourly"}`); rec.Code != 400 {
		t.Fatalf("invalid schedule: %d", rec.Code)
	}
	if rec := e.send("PATCH", imagePath(org, bare), aliceTok, `{"autoOnBaseChange":true}`); rec.Code != 422 {
		t.Fatalf("no recipe: %d", rec.Code)
	}
	if rec := e.send("PATCH", imagePath(org, img), bobTok, `{"autoSchedule":"daily"}`); rec.Code != 403 {
		t.Fatalf("without build.builds.create: %d", rec.Code)
	}
	if rec := e.send("PATCH", imagePath(org, img), bobTok, `{"description":"still editable"}`); rec.Code != 200 {
		t.Fatalf("plain edit: %d", rec.Code)
	}
	rec := e.send("PATCH", imagePath(org, img), aliceTok, `{"autoSchedule":"weekly","autoOnBaseChange":true}`)
	var got imageJSON
	decode(t, rec, &got)
	if rec.Code != 200 || got.AutoSchedule != "weekly" || !got.AutoOnBaseChange || got.AutoError != "" || got.AutoLastAt != nil {
		t.Fatalf("set: %d %+v", rec.Code, got)
	}
	if i := e.image(img); i.AutoBy == nil || *i.AutoBy != alice.ID || i.Description != "still editable" {
		t.Fatalf("stored: %+v", i)
	}
	e.setAuto(aliceTok, org, img, `{"autoSchedule":""}`)
	if i := e.image(img); i.AutoSchedule != "" || !i.AutoOnBaseChange {
		t.Fatalf("partial update: %+v", i)
	}
	if es := e.audit(store.ActionImageUpdate); es[0].Details["autoSchedule"] != "" || es[0].Details["autoOnBaseChange"] != true {
		t.Fatalf("audit: %+v", es[0].Details)
	}

	// Turning automation off clears a stale error.
	e.exec("UPDATE images SET auto_error = 'RUN failed' WHERE id = $1", img)
	e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":false}`)
	if i := e.image(img); i.AutoError != "" {
		t.Fatalf("error kept: %q", i.AutoError)
	}
}

func TestAutoScheduleDue(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "basis", "")
	off := e.recipeImage(aliceTok, org, "manual", "")
	e.setAuto(aliceTok, org, img, `{"autoSchedule":"daily"}`)

	// Never built: due at once, and never twice while the build is queued.
	e.autoTick()
	e.autoTick()
	if bs := e.builds(img); len(bs) != 1 || !bs[0].Automatic || bs[0].StartedBy != alice.ID || bs[0].State != store.BuildQueued {
		t.Fatalf("builds: %+v", bs)
	}
	if bs := e.builds(off); len(bs) != 0 {
		t.Fatalf("image without automation built: %+v", bs)
	}
	if b := e.finish(org, img, 1, `[]`, ""); b.State != store.BuildSucceeded || e.latest(img) != digestN(1) {
		t.Fatalf("build: %+v latest %s", b, e.latest(img))
	}
	if i := e.image(img); i.AutoLastAt == nil || i.AutoError != "" {
		t.Fatalf("auto result: %+v", i)
	}
	rec := e.do("GET", buildsPath(org, img), aliceTok)
	if !strings.Contains(rec.Body.String(), `"automatic":true`) {
		t.Fatalf("build json: %s", rec.Body.String())
	}

	e.now = e.now.Add(23 * time.Hour)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 1 {
		t.Fatalf("built before a day passed: %d", len(bs))
	}
	e.now = e.now.Add(2 * time.Hour)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 || !bs[0].Automatic {
		t.Fatalf("not rebuilt after a day: %+v", bs)
	}
}

func TestAutoBaseChange(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "basis", "")
	ubuntu := "index.docker.io/library/ubuntu:24.04"
	e.bases[ubuntu] = digestN(0x100)

	// A manual build records its bases; nothing changed, so no automatic build.
	e.startBuild(aliceTok, org, img)
	e.finish(org, img, 1, `[{"ref":"ubuntu:24.04","digest":"`+digestN(0x100)+`"}]`, "")
	v, err := e.store.ImageVersion(context.Background(), img, digestN(1))
	if err != nil || len(v.BaseDigests) != 1 || v.BaseDigests[0].Ref != "ubuntu:24.04" {
		t.Fatalf("version bases: %+v %v", v, err)
	}
	e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":true}`)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 1 || e.looks != 1 || e.image(img).AutoCheckedAt == nil {
		t.Fatalf("unchanged base built: %d builds, %d lookups", len(bs), e.looks)
	}

	// The base moves: at most one check an hour.
	e.bases[ubuntu] = digestN(0x101)
	e.now = e.now.Add(30 * time.Minute)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 1 || e.looks != 1 {
		t.Fatalf("checked within the hour: %d builds, %d lookups", len(bs), e.looks)
	}
	e.now = e.now.Add(31 * time.Minute)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 || !bs[0].Automatic {
		t.Fatalf("base change did not build: %+v", bs)
	}

	// A failed automatic build keeps latest and shows the error; at most one automatic build in 6 h.
	if b := e.finish(org, img, 2, "", "RUN apt-get failed"); b.State != store.BuildFailed {
		t.Fatalf("build: %+v", b)
	}
	if i := e.image(img); i.AutoError != "RUN apt-get failed" || e.latest(img) != digestN(1) {
		t.Fatalf("after failure: %q latest %s", i.AutoError, e.latest(img))
	}
	e.now = e.now.Add(2 * time.Hour)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 {
		t.Fatalf("rebuilt within 6 h: %d", len(bs))
	}
	e.now = e.now.Add(5 * time.Hour)
	e.autoTick()
	if b := e.finish(org, img, 3, `[{"ref":"ubuntu:24.04","digest":"`+digestN(0x101)+`"}]`, ""); b.State != store.BuildSucceeded || !b.Automatic {
		t.Fatalf("retry: %+v", b)
	}
	if i := e.image(img); i.AutoError != "" || e.latest(img) != digestN(3) {
		t.Fatalf("success did not clear: %q latest %s", i.AutoError, e.latest(img))
	}
	rec := e.do("GET", imagePath(org, img), aliceTok)
	var got imageJSON
	decode(t, rec, &got)
	if got.AutoError != "" || got.AutoLastAt == nil || !got.AutoOnBaseChange {
		t.Fatalf("image json: %+v", got)
	}
}

func TestAutoCheckErrorBuildsNothing(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "basis", "")
	e.startBuild(aliceTok, org, img)
	e.finish(org, img, 1, `[{"ref":"alpine:3","digest":"`+digestN(0x100)+`"}]`, "")
	e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":true}`)

	e.autoTick()
	i := e.image(img)
	if !strings.HasPrefix(i.AutoError, autoCheckPrefix) || !strings.Contains(i.AutoError, "registry unavailable") || i.AutoCheckedAt != nil {
		t.Fatalf("check error: %+v", i)
	}
	// Retried on the next pass, not an hour later.
	e.now = e.now.Add(10 * time.Minute)
	e.autoTick()
	if e.looks != 2 || len(e.builds(img)) != 1 {
		t.Fatalf("lookups %d, builds %d", e.looks, len(e.builds(img)))
	}
	e.bases["index.docker.io/library/alpine:3"] = digestN(0x100)
	e.now = e.now.Add(10 * time.Minute)
	e.autoTick()
	if i := e.image(img); i.AutoError != "" || len(e.builds(img)) != 1 {
		t.Fatalf("good check: %q, %d builds", i.AutoError, len(e.builds(img)))
	}
}

func TestAutoHubBase(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	base := e.createImage(aliceTok, org, `{"name":"base","scope":"org"}`)
	e.addVersion(base, 0x200, "latest")
	img := e.recipeImage(aliceTok, org, "app", "")
	ref := "hub.internal:8090/i/" + base.String() + ":latest"
	e.startBuild(aliceTok, org, img)
	e.finish(org, img, 1, `[{"ref":"`+ref+`","digest":"`+digestN(0x200)+`"}]`, "")
	e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":true}`)

	e.autoTick()
	if len(e.builds(img)) != 1 || e.looks != 0 || e.image(img).AutoError != "" {
		t.Fatalf("unchanged hub base: %d builds, %d registry lookups, %q", len(e.builds(img)), e.looks, e.image(img).AutoError)
	}
	e.addVersion(base, 0x201, "latest")
	e.now = e.now.Add(time.Hour)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 || !bs[0].Automatic {
		t.Fatalf("new base version did not build: %+v", bs)
	}
}

func TestAutoRunsWithStarterRights(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.node(org)
	e.createEnv(aliceTok, org, `{"name":"ORG_TOKEN","value":"v","kind":"secret","scope":"org"}`)
	img := e.recipeImage(aliceTok, org, "basis", `[{"kind":"secret","scope":"org","name":"ORG_TOKEN"}]`)
	e.setAuto(bobTok, org, img, `{"autoSchedule":"weekly"}`)
	deny := e.createRole(org, `{"name":"no secrets","rules":[{"effect":"deny","object":"org/env","action":"secrets.variables.use"}]}`)
	e.bind(org, authz.UserSubject(bob.ID), deny)

	e.autoTick()
	bs := e.builds(img)
	if len(bs) != 1 || bs[0].State != store.BuildFailed || bs[0].StartedBy != bob.ID || !strings.Contains(bs[0].Error, "run as bob: no right to use secret org/ORG_TOKEN") {
		t.Fatalf("builds: %+v", bs)
	}
	if i := e.image(img); i.AutoError != bs[0].Error || i.AutoSchedule != "weekly" {
		t.Fatalf("image: %+v", i)
	}

	// The deleted secret of the example: the build fails naming it, latest stays.
	e.setAuto(aliceTok, org, img, `{"autoSchedule":"weekly"}`)
	e.exec("DELETE FROM env_vars WHERE name = 'ORG_TOKEN'")
	e.now = e.now.Add(7 * time.Hour)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 || !strings.Contains(bs[0].Error, "no longer exists") || e.latest(img) != "" {
		t.Fatalf("deleted secret: %+v", bs)
	}
}

func TestBuildResultRejectsInvalidBases(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.recipeImage(aliceTok, org, "basis", "")
	b, secret := e.running(aliceTok, org, img)
	for _, bases := range []string{`[{"ref":"","digest":"` + digestN(1) + `"}]`, `[{"ref":"alpine","digest":"latest"}]`} {
		if rec := e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"digest":"`+digestN(2)+`","bases":`+bases+`}`); rec.Code != 400 {
			t.Fatalf("bases %s: %d", bases, rec.Code)
		}
	}
}

func TestAutoEditorCannotBorrowRefs(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.node(org)
	e.createEnv(aliceTok, org, `{"name":"ORG_TOKEN","value":"v","kind":"secret","scope":"org"}`)
	refs := `[{"kind":"secret","scope":"org","name":"ORG_TOKEN"}]`
	img := e.recipeImage(aliceTok, org, "basis", refs)
	e.setAuto(aliceTok, org, img, `{"autoSchedule":"weekly"}`)
	editOnly := e.createRole(org, `{"name":"edit only","rules":[{"effect":"deny","object":"org/images","action":"build.builds.create"},{"effect":"deny","object":"org/env","action":"secrets.variables.use"}]}`)
	e.bind(org, authz.UserSubject(bob.ID), editOnly)
	// "RlJPTSBhbHBpbmU=" is "FROM alpine"; the references stay as alice added them.
	if rec := e.send("POST", imagePath(org, img)+"/recipe/edits", bobTok, `{"base":1,"put":[{"path":"Dockerfile","content":"RlJPTSBhbHBpbmU="}]}`); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}

	var shown imageJSON
	decode(t, e.do("GET", imagePath(org, img), aliceTok), &shown)
	if !shown.AutoNeedsApproval || shown.AutoRevision != nil || *shown.RecipeRevision != 2 {
		t.Fatalf("approval not asked for: %+v", shown)
	}

	e.autoTick()
	e.tick()
	bs := e.builds(img)
	if len(bs) != 1 || bs[0].State != store.BuildFailed || bs[0].VMID != nil || bs[0].Revision != 2 ||
		!strings.Contains(bs[0].Error, "recipe revision 2 by bob: no right to build the image — approve the revision") {
		t.Fatalf("builds: %+v", bs)
	}
	if i := e.image(img); i.AutoError != bs[0].Error || i.AutoSchedule != "weekly" {
		t.Fatalf("image: %+v", i)
	}

	// Saving settings vouches for nothing.
	e.setAuto(aliceTok, org, img, `{"autoSchedule":"weekly","autoOnBaseChange":true}`)
	e.now = e.now.Add(7 * time.Hour)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 || bs[0].State != store.BuildFailed {
		t.Fatalf("settings change vouched: %+v", bs)
	}

	// Only the revision the client showed can be approved.
	rec := e.send("PATCH", imagePath(org, img), aliceTok, `{"approveRevision":1}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "recipe_changed") {
		t.Fatalf("stale approval: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PATCH", imagePath(org, img), bobTok, `{"approveRevision":2}`); rec.Code != 403 {
		t.Fatalf("approval without build.builds.create: %d", rec.Code)
	}
	rec = e.send("PATCH", imagePath(org, img), aliceTok, `{"approveRevision":2}`)
	decode(t, rec, &shown)
	if rec.Code != 200 || shown.AutoNeedsApproval || shown.AutoRevision == nil || *shown.AutoRevision != 2 || shown.AutoError != "" {
		t.Fatalf("approve: %d %+v", rec.Code, shown)
	}
	if es := e.audit(store.ActionImageUpdate); es[0].Details["approvedRevision"] != float64(2) {
		t.Fatalf("audit: %+v", es[0].Details)
	}
	// The approval answers the failures: the next build runs at once, not 6 h after the last one.
	e.autoTick()
	if bs := e.builds(img); len(bs) != 3 || bs[0].State != store.BuildQueued || bs[0].Revision != 2 {
		t.Fatalf("after approval: %+v", bs)
	}
}

func TestAutoCheckErrorKeepsBuildError(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "basis", "")
	e.startBuild(aliceTok, org, img)
	e.finish(org, img, 1, `[{"ref":"alpine:3","digest":"`+digestN(0x100)+`"}]`, "")
	e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":true}`)
	e.exec("UPDATE images SET auto_error = 'RUN failed', auto_last_at = NULL WHERE id = $1", img)

	e.autoTick()
	if i := e.image(img); i.AutoError != "RUN failed" || i.AutoLastAt != nil {
		t.Fatalf("check error replaced the build error: %+v", i)
	}
	if e.looks != 1 {
		t.Fatalf("lookups %d", e.looks)
	}
}

func TestAutoFailedLookupCachedPerPass(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	bases := `[{"ref":"alpine:3","digest":"` + digestN(0x100) + `"}]`
	for _, n := range []string{"one", "two"} {
		img := e.recipeImage(aliceTok, org, n, "")
		e.startBuild(aliceTok, org, img)
		e.finish(org, img, 1, bases, "")
		e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":true}`)
	}
	e.autoTick()
	if e.looks != 1 {
		t.Fatalf("lookups %d", e.looks)
	}
}

func TestTrimError(t *testing.T) {
	if got := trimError(errors.New("GET x: UNAUTHORIZED\n{\"body\":\"secret\"}")); got != "GET x: UNAUTHORIZED" {
		t.Fatalf("got %q", got)
	}
	if got := trimError(errors.New(strings.Repeat("é", 300))); len([]rune(got)) != autoErrorMax+1 {
		t.Fatalf("not trimmed: %d", len([]rune(got)))
	}
}

func TestAutoApprovalIsPersonal(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.addMember(org, carol, store.RoleAdmin)
	e.node(org)
	e.createEnv(aliceTok, org, `{"name":"ORG_TOKEN","value":"v","kind":"secret","scope":"org"}`)
	img := e.recipeImage(aliceTok, org, "basis", `[{"kind":"secret","scope":"org","name":"ORG_TOKEN"}]`)
	e.setAuto(aliceTok, org, img, `{"autoSchedule":"weekly","approveRevision":1}`)
	editOnly := e.createRole(org, `{"name":"edit only","rules":[{"effect":"deny","object":"org/images","action":"build.builds.create"},{"effect":"deny","object":"org/env","action":"secrets.variables.use"}]}`)
	e.bind(org, authz.UserSubject(bob.ID), editOnly)
	if rec := e.send("POST", imagePath(org, img)+"/recipe/edits", bobTok, `{"base":1,"put":[{"path":"Dockerfile","content":"RlJPTSBhbHBpbmU="}]}`); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	e.setAuto(aliceTok, org, img, `{"approveRevision":2}`)

	// Alice's own settings change keeps her approval.
	e.setAuto(aliceTok, org, img, `{"autoOnBaseChange":true}`)
	var shown imageJSON
	decode(t, e.do("GET", imagePath(org, img), aliceTok), &shown)
	if shown.AutoNeedsApproval || shown.AutoRevision == nil || *shown.AutoRevision != 2 {
		t.Fatalf("own change dropped the approval: %+v", shown)
	}

	// Carol's settings change does not inherit it.
	e.setAuto(carolTok, org, img, `{"autoSchedule":"daily"}`)
	decode(t, e.do("GET", imagePath(org, img), carolTok), &shown)
	if !shown.AutoNeedsApproval || shown.AutoRevision != nil {
		t.Fatalf("carol inherited alice's approval: %+v", shown)
	}
	if i := e.image(img); i.AutoBy == nil || *i.AutoBy != carol.ID || i.AutoApprovedAt != nil {
		t.Fatalf("image: %+v", i)
	}
	e.autoTick()
	if bs := e.builds(img); len(bs) != 1 || bs[0].State != store.BuildFailed || !strings.Contains(bs[0].Error, "recipe revision 2 by bob") {
		t.Fatalf("builds: %+v", bs)
	}

	// Carol approves herself; a settings change carrying the approval re-approves as the saver.
	e.setAuto(carolTok, org, img, `{"autoSchedule":"daily","approveRevision":2}`)
	e.autoTick()
	if bs := e.builds(img); len(bs) != 2 || bs[0].State != store.BuildQueued || bs[0].StartedBy != carol.ID {
		t.Fatalf("after carol's approval: %+v", bs)
	}
}
