package iam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/platform/permission"
)

func (e *env) createOrg(jwt, name string) uuid.UUID {
	e.t.Helper()
	res, err := e.client.CreateOrg(context.Background(), req(jwt, &iamv1.CreateOrgRequest{Name: name}))
	if err != nil {
		e.t.Fatalf("create org: %v", err)
	}
	return uuid.MustParse(res.Msg.Org.Id)
}

func (e *env) getOrg(jwt string, org uuid.UUID) (*iamv1.GetOrgResponse, error) {
	res, err := e.client.GetOrg(context.Background(), req(jwt, &iamv1.GetOrgRequest{Org: org.String()}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestCreateOrgMakesCallerOwner(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	got := must[iamv1.CreateOrgResponse](t)(e.client.CreateOrg(context.Background(), req(tok, &iamv1.CreateOrgRequest{Name: "  Acme  "}))).Org
	if got.Name != "Acme" || got.Role != "owner" || got.Personal || got.Id == "" {
		t.Fatalf("org: %+v", got)
	}
	es := e.audit(store.ActionOrgCreate)
	if len(es) != 1 || es[0].ActorID != u.ID.String() || es[0].OrgID == nil || es[0].OrgID.String() != got.Id || es[0].Resource != "org/"+got.Id {
		t.Fatalf("audit: %+v", es)
	}
	// The new membership is visible to the enforcer without a manual reload.
	if !e.z.Enforcer().Allowed(authz.UserSubject(u.ID), uuid.MustParse(got.Id), authz.ObjAudit, "audit.events.list") {
		t.Fatal("owner cannot read the audit trail")
	}
	for _, name := range []string{"   ", strings.Repeat("x", 65), "a\u0000b", "a\nb", ""} {
		_, err := e.client.CreateOrg(context.Background(), req(tok, &iamv1.CreateOrgRequest{Name: name}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	if _, err := e.client.CreateOrg(context.Background(), req(tok, &iamv1.CreateOrgRequest{Name: strings.Repeat("ä", 64)})); err != nil {
		t.Fatalf("64 runes: %v", err)
	}
}

func TestGetOrgPermissions(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)

	memberPerms := []string{"telemetry.dashboards.get", "compute.disks.create", "secrets.variables.list", "secrets.variables.use", "artifacts.images.list", "artifacts.images.use", "secrets.integrations.list", "secrets.integrations.use", "quota.limits.get", "iam.members.list", "network.networks.create", "network.networks.get", "network.networks.use", "compute.nodes.get", "compute.nodes.use", "iam.roles.list", "iam.teams.leave", "iam.teams.list", "compute.instances.create"}
	adminPerms := []string{"audit.events.list", "audit.settings.get", "audit.settings.update", "telemetry.dashboards.create", "telemetry.dashboards.update", "telemetry.dashboards.get", "network.devices.delete", "network.devices.list", "compute.disks.create", "compute.disks.delete", "compute.disks.get", "compute.disks.update", "secrets.variables.delete", "secrets.variables.list", "secrets.variables.set", "secrets.variables.use", "build.builds.create", "artifacts.images.setDefault", "artifacts.images.update", "artifacts.images.list", "artifacts.images.use", "secrets.integrations.delete", "secrets.integrations.list", "secrets.integrations.set", "secrets.integrations.use", "quota.limits.get", "quota.limits.update", "telemetry.logEntries.list", "iam.invites.create", "iam.members.list", "iam.members.delete",
		"network.networks.create", "network.networks.delete", "network.networks.get", "network.networks.use", "compute.nodes.create", "compute.nodes.delete", "compute.nodes.getMetrics", "compute.nodes.get", "compute.nodes.update", "compute.nodes.use", "iam.orgs.update",
		"iam.roleBindings.manage", "iam.roles.list", "iam.roles.manage", "iam.teams.create", "iam.teams.delete", "iam.teams.leave", "iam.teams.manageMembers", "iam.teams.list", "iam.teams.update", "telemetry.tokens.create",
		"compute.instances.create", "compute.instances.delete", "compute.instances.setDisks", "compute.instances.getMetrics", "compute.instances.setNetworks", "compute.instances.setPower", "compute.instances.get", "compute.instances.restore", "compute.instances.snapshot"}
	slices.Sort(memberPerms)
	slices.Sort(adminPerms)
	ownerPerms := append(slices.Clone(adminPerms), "iam.members.setRole")
	slices.Sort(ownerPerms)

	for _, c := range []struct {
		tok, role string
		want      []string
	}{{ownerTok, "owner", ownerPerms}, {adminTok, "admin", adminPerms}, {memberTok, "member", memberPerms}} {
		got, err := e.getOrg(c.tok, org)
		if err != nil {
			t.Fatalf("%s: %v", c.role, err)
		}
		if got.Org.Id != org.String() || got.Org.Name != "Acme" || got.Org.Role != c.role || got.Org.Personal || !slices.Equal(got.Permissions, c.want) {
			t.Fatalf("%s: %+v", c.role, got)
		}
	}

	// A team admin may manage only their own team, which doesn't cover org/teams/*.
	var team uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO teams (org_id, name) VALUES ($1, 't') RETURNING id`, org).Scan(&team); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO team_members (team_id, user_id, admin) VALUES ($1, $2, true)`, team, member.ID)
	e.reload()
	if got, err := e.getOrg(memberTok, org); err != nil || !slices.Equal(got.Permissions, memberPerms) {
		t.Fatalf("team admin: %+v %v", got, err)
	}

	// Denying everything leaves no permissions, but GetOrg needs membership only.
	e.bindRole(org, authz.UserSubject(member.ID), "deny", "*", "*")
	if got, err := e.getOrg(memberTok, org); err != nil || len(got.Permissions) != 0 || got.Org.Role != "member" {
		t.Fatalf("deny all: %+v %v", got, err)
	}
}

func TestGetOrgNotMemberNotFound(t *testing.T) {
	e := newEnv(t)
	platformAdmin, adminTok := e.user("alice")
	if !platformAdmin.PlatformAdmin {
		t.Fatal("first user should be platform admin")
	}
	_, bobTok := e.user("bob")
	org := e.createOrg(bobTok, "Bob Inc")
	_, err := e.getOrg(adminTok, org)
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.client.UpdateOrg(context.Background(), req(adminTok, &iamv1.UpdateOrgRequest{Org: org.String(), Name: "x"}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.getOrg(bobTok, uuid.New())
	wantCode(t, err, connect.CodeNotFound)
	for _, id := range []string{"nope", strings.ToUpper(org.String()), "{" + org.String() + "}"} {
		_, err := e.client.GetOrg(context.Background(), req(bobTok, &iamv1.GetOrgRequest{Org: id}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
}

func TestGetOrgUsesPolicy(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.exec(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org, bob.ID)
	_, err := e.getOrg(bobTok, org)
	wantCode(t, err, connect.CodeNotFound)
	e.reload()
	if got, err := e.getOrg(bobTok, org); err != nil || got.Org.Name != "Acme" || got.Org.Role != "member" {
		t.Fatalf("after reload: %+v %v", got, err)
	}
}

func TestUpdateOrg(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	ctx := context.Background()

	got := must[iamv1.UpdateOrgResponse](t)(e.client.UpdateOrg(ctx, req(adminTok, &iamv1.UpdateOrgRequest{Org: org.String(), Name: " Acme Inc "}))).Org
	if got.Name != "Acme Inc" || got.Role != "admin" {
		t.Fatalf("admin rename: %+v", got)
	}
	es := e.audit("iam.orgs.update")
	if len(es) != 1 || es[0].ActorID != admin.ID.String() || *es[0].OrgID != org || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "Acme Inc" || es[0].Resource != "org/"+org.String() {
		t.Fatalf("audit: %+v", es)
	}

	_, err := e.client.UpdateOrg(ctx, req(memberTok, &iamv1.UpdateOrgRequest{Org: org.String(), Name: "Hacked"}))
	wantCode(t, err, connect.CodePermissionDenied)
	es = e.audit("iam.orgs.update")
	if len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() || *es[0].OrgID != org || es[0].Resource != "org/settings" {
		t.Fatalf("denied audit: %+v", es)
	}
	if o, _ := e.getOrg(ownerTok, org); o.Org.Name != "Acme Inc" {
		t.Fatalf("name after denied rename: %q", o.Org.Name)
	}

	_, err = e.client.UpdateOrg(ctx, req(adminTok, &iamv1.UpdateOrgRequest{Org: org.String(), Name: ""}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = e.client.UpdateOrg(ctx, req(ownerTok, &iamv1.UpdateOrgRequest{Org: e.personalOrg(owner).String(), Name: "x"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}

func TestCommitRejectsInvalidPolicy(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'r') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.reload()
	before := e.z.Enforcer()
	// A poison row committed behind the API's back: any write that would commit it must fail.
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, 'bogus', $2)`, org, role)

	s := &server{Deps: Deps{Store: e.store, Policy: e.z}}
	err := s.commit(context.Background(), func(tx *store.Store) error {
		return tx.Audit(context.Background(), store.AuditEntry{ActorKind: store.ActorUser, ActorID: u.ID.String(), Action: "test.poison", Result: store.ResultAllowed})
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "invalid policy") || strings.Contains(err.Error(), "bogus") {
		t.Fatalf("commit: %v", err)
	}
	if len(e.audit("test.poison")) != 0 {
		t.Fatal("audit row of rolled back tx stored")
	}
	if e.z.Enforcer() != before {
		t.Fatal("enforcer swapped")
	}
}

// restDo sends a REST request through the transcoder with an optional JWT.
func (e *env) restDo(method, path, jwt string) (int, map[string]any) {
	e.t.Helper()
	r, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if jwt != "" {
		r.Header.Set("Authorization", "Bearer "+jwt)
	}
	res, err := e.srv.Client().Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	return res.StatusCode, body
}

// restSend sends a REST request with a JSON body and returns the status and error message.
func (e *env) restSend(method, path, jwt, body string) (int, string) {
	e.t.Helper()
	r, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+jwt)
	r.Header.Set("Content-Type", "application/json")
	res, err := e.srv.Client().Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct{ Message string }
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out.Message
}

func TestRESTPathOrgWinsOverQuery(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, _ := e.user("carol")
	orgA := e.createOrg(aliceTok, "A")
	orgB := e.createOrg(bobTok, "B")
	e.addMember(orgA, bob, store.RoleMember)
	e.addMember(orgA, carol, store.RoleMember)
	e.addMember(orgB, carol, store.RoleMember)

	// The transcoder would let ?org= override the path, so a query is refused before it decodes anything.
	if code, body := e.restDo("GET", "/api/iam/v1/orgs/"+orgA.String()+"?org="+orgB.String(), bobTok); code != http.StatusBadRequest {
		t.Fatalf("get: %d %v", code, body)
	}
	if code, body := e.restDo("DELETE", "/api/iam/v1/orgs/"+orgA.String()+"/members/"+carol.ID.String()+"?org="+orgB.String(), bobTok); code != http.StatusBadRequest {
		t.Fatalf("delete: %d %v", code, body)
	}
	if e.role(orgA, carol) != store.RoleMember || e.role(orgB, carol) != store.RoleMember {
		t.Fatal("carol removed")
	}
	// A body field cannot override the path either: bob owns B but is only a member of A.
	r, err := http.NewRequest("PATCH", e.srv.URL+"/api/iam/v1/orgs/"+orgA.String(), strings.NewReader(`{"org":"`+orgB.String()+`","name":"hacked"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+bobTok)
	r.Header.Set("Content-Type", "application/json")
	res, err := e.srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if es := e.audit("iam.orgs.update"); res.StatusCode != http.StatusForbidden || len(es) != 1 || *es[0].OrgID != orgA || es[0].Result != store.ResultDenied {
		t.Fatalf("patch: %d %+v", res.StatusCode, es)
	}
	if code, body := e.restDo("GET", "/api/iam/v1/orgs/"+orgA.String(), bobTok); code != http.StatusOK || body["org"].(map[string]any)["id"] != orgA.String() {
		t.Fatalf("get without query: %d %v", code, body)
	}
}

func TestRESTDeleteMember(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	target, _ := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	e.addMember(org, target, store.RoleMember)
	path := "/api/iam/v1/orgs/" + org.String() + "/members/" + target.ID.String()

	if code, _ := e.restDo("DELETE", path, ""); code != http.StatusUnauthorized {
		t.Fatalf("no jwt: %d", code)
	}
	if code, _ := e.restDo("DELETE", path, memberTok); code != http.StatusForbidden {
		t.Fatalf("member: %d", code)
	}
	if es := e.audit(store.ActionMemberRemove); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	if code, body := e.restDo("DELETE", path, ownerTok); code != http.StatusOK {
		t.Fatalf("owner: %d %v", code, body)
	}
	if e.role(org, target) != "" {
		t.Fatal("still a member")
	}
}

// Every annotation must name a known action on its catalog object, so the catalog can describe the whole API.
func TestAnnotationsInCatalog(t *testing.T) {
	known := map[authz.Permission]bool{}
	for _, p := range append(slices.Clone(authz.Catalog), authz.Unruled...) {
		known[p] = true
	}
	methods := iamv1.File_observatory_iam_v1_iam_proto.Services().ByName("IAMService").Methods()
	for i := range methods.Len() {
		md := methods.Get(i)
		perm, ok := permission.Of(md)
		if !ok {
			t.Errorf("%s: no annotation", md.Name())
			continue
		}
		if p := (authz.Permission{Action: perm.Name, Object: catalogObject(perm.Object)}); !known[p] {
			t.Errorf("%s: %s on %s is not in the catalog", md.Name(), p.Action, p.Object)
		}
		if org := md.Input().Fields().ByName("org"); org == nil && strings.HasPrefix(perm.Object, "org") {
			t.Errorf("%s: org object without org field", md.Name())
		}
	}
}

// catalogObject turns an annotation object into its catalog pattern: each {field} becomes *.
func catalogObject(obj string) string {
	var b strings.Builder
	for {
		before, after, ok := strings.Cut(obj, "{")
		b.WriteString(before)
		if !ok {
			return b.String()
		}
		_, obj, _ = strings.Cut(after, "}")
		b.WriteString("*")
	}
}

// countingBody yields n bytes of JSON-ish filler and counts how many the handler pulled.
type countingBody struct{ n, read int }

func (b *countingBody) Read(p []byte) (int, error) {
	if b.read >= b.n {
		return 0, io.EOF
	}
	k := min(len(p), b.n-b.read)
	for i := range p[:k] {
		p[i] = ' '
	}
	b.read += k
	return k, nil
}

func TestOversizedBodyRefusedBeforeAuth(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ name, path, protocol string }{
		{"connect", "/api/iam/v1/observatory.iam.v1.IAMService/CreateOrg", "1"},
		{"rest", "/api/iam/v1/orgs", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := &countingBody{n: 64 << 20}
			r := httptest.NewRequest(http.MethodPost, c.path, body)
			r.Header.Set("Content-Type", "application/json")
			if c.protocol != "" {
				r.Header.Set("Connect-Protocol-Version", c.protocol)
			}
			w := httptest.NewRecorder()
			e.srv.Config.Handler.ServeHTTP(w, r)
			// resource_exhausted, over Connect and REST alike.
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status %d, want 429: %s", w.Code, w.Body)
			}
			if body.read > 2<<20 {
				t.Fatalf("handler read %d bytes before refusing", body.read)
			}
		})
	}
}
