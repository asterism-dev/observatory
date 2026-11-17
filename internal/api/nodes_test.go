package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func nodesPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/nodes" }

type nodeCreated struct {
	Node           struct{ ID, Name, State string }
	InstallCommand string
}

func (e *env) createNode(tok string, org uuid.UUID, name string) nodeCreated {
	e.t.Helper()
	rec := e.send("POST", nodesPath(org), tok, `{"name":"`+name+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("create node: %d %s", rec.Code, rec.Body.String())
	}
	var got nodeCreated
	decode(e.t, rec, &got)
	return got
}

func (e *env) enrolledNode(tok string, org uuid.UUID, name string) string {
	e.t.Helper()
	c := e.createNode(tok, org, name)
	if rec := e.enroll(tokenOf(c.InstallCommand)); rec.Code != 200 {
		e.t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	return c.Node.ID
}

func tokenOf(cmd string) string { return cmd[strings.LastIndex(cmd, " ")+1:] }

func (e *env) enroll(token string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	NodeEndpoints(mux, e.store, e.newFleet())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/nodes/enroll", strings.NewReader(`{"token":"`+token+`"}`)))
	return rec
}

func TestNodeLifecycleAndAudit(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")

	c := e.createNode(tok, org, "n1")
	if c.Node.State != "pending" || !strings.HasPrefix(c.InstallCommand, "curl -fsSL https://hub.test/node/install.sh | sudo sh -s -- --hub https://hub.test --token ") {
		t.Fatalf("%+v", c)
	}
	token := tokenOf(c.InstallCommand)
	if rec := e.send("POST", nodesPath(org), tok, `{"name":"n1"}`); rec.Code != 409 {
		t.Fatalf("duplicate name: %d", rec.Code)
	}

	rec := e.enroll(token)
	if rec.Code != 200 {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	var enrolled struct{ NodeID, Credential string }
	decode(t, rec, &enrolled)
	if enrolled.NodeID != c.Node.ID || enrolled.Credential == "" {
		t.Fatalf("%+v", enrolled)
	}
	if rec := e.enroll(token); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid enrollment token") {
		t.Fatalf("second enroll: %d %s", rec.Code, rec.Body.String())
	}

	var list []struct {
		ID, Name, State      string
		CanUpdate, CanDelete bool
	}
	decode(t, e.do("GET", nodesPath(org), tok), &list)
	if len(list) != 1 || list[0].State != "offline" || !list[0].CanUpdate || !list[0].CanDelete {
		t.Fatalf("%+v", list)
	}
	if rec := e.send("PATCH", nodesPath(org)+"/"+c.Node.ID, tok, `{"name":"n2"}`); rec.Code != 204 {
		t.Fatalf("rename: %d", rec.Code)
	}
	rec = e.do("POST", nodesPath(org)+"/"+c.Node.ID+"/credential", tok)
	if rec.Code != 200 {
		t.Fatalf("credential: %d %s", rec.Code, rec.Body.String())
	}
	var regen struct{ InstallCommand string }
	decode(t, rec, &regen)
	if e.enroll(tokenOf(regen.InstallCommand)).Code != 200 {
		t.Fatal("re-enroll after revoke")
	}
	if rec := e.do("DELETE", nodesPath(org)+"/"+c.Node.ID, tok); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}

	for _, action := range []string{"node:create", "node:update", "node:enroll", "node:revoke", "node:delete"} {
		es := e.audit(action)
		if len(es) == 0 {
			t.Fatalf("no %s audit row", action)
		}
		for _, en := range es {
			for _, secret := range []string{token, tokenOf(regen.InstallCommand), enrolled.Credential} {
				if strings.Contains(strings.Join(append([]string{en.Resource, en.ActorID}, detailStrings(en.Details)...), " "), secret) {
					t.Fatalf("%s audit leaks a secret", action)
				}
			}
		}
	}
	if es := e.audit("node:enroll"); es[0].ActorKind != store.ActorNode || es[0].ActorID != c.Node.ID {
		t.Fatalf("enroll actor: %+v", es[0])
	}
}

func TestRegeneratePendingNodeAuditedAsUpdate(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	c := e.createNode(tok, org, "n1")
	if rec := e.do("POST", nodesPath(org)+"/"+c.Node.ID+"/credential", tok); rec.Code != 200 {
		t.Fatalf("credential: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("node:revoke"); len(es) != 0 {
		t.Fatalf("pending node audited as revoke: %+v", es)
	}
	if es := e.audit("node:update"); len(es) != 1 || es[0].Details["regenerated"] != true {
		t.Fatalf("update audit: %+v", es)
	}
}

func detailStrings(d map[string]any) []string {
	var out []string
	for _, v := range d {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestNodePermissionsAndIsolation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	_, eveTok := e.user("eve")
	org := e.createOrg(ownerTok, "Acme")
	other := e.createOrg(eveTok, "Other")
	e.addMember(org, bob, store.RoleMember)
	n := e.createNode(ownerTok, org, "n1")
	id := n.Node.ID

	if rec := e.send("POST", nodesPath(org), bobTok, `{"name":"x"}`); rec.Code != 403 {
		t.Fatalf("member create: %d", rec.Code)
	}
	if len(e.audit("node:create")) == 0 || e.audit("node:create")[0].Result != store.ResultDenied {
		t.Fatalf("denied create not audited first: %+v", e.audit("node:create"))
	}
	for _, c := range [][2]string{{"PATCH", ""}, {"POST", "/credential"}, {"DELETE", ""}} {
		if rec := e.send(c[0], nodesPath(org)+"/"+id+c[1], bobTok, `{"name":"x"}`); rec.Code != 403 {
			t.Fatalf("member %s%s: %d", c[0], c[1], rec.Code)
		}
	}
	if rec := e.do("GET", nodesPath(org)+"/"+id, bobTok); rec.Code != 200 {
		t.Fatalf("member read: %d", rec.Code)
	}

	if rec := e.do("GET", nodesPath(org), eveTok); rec.Code != 404 {
		t.Fatalf("outsider list: %d", rec.Code)
	}
	for _, c := range [][2]string{{"GET", ""}, {"PATCH", ""}, {"POST", "/credential"}, {"DELETE", ""}} {
		if rec := e.send(c[0], nodesPath(other)+"/"+id+c[1], eveTok, `{"name":"x"}`); rec.Code != 404 {
			t.Fatalf("other org node %s%s: %d", c[0], c[1], rec.Code)
		}
	}
}

func TestNodeJSONShowsUnderlay(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.enrolledNode(tok, org, "n1")
	read := func() (n struct{ Underlay *underlayJSON }) {
		decode(t, e.do("GET", nodesPath(org)+"/"+id, tok), &n)
		return
	}
	if read().Underlay != nil {
		t.Fatal("underlay before enrollment")
	}
	node, _ := uuid.Parse(id)
	ip, err := e.store.EnrollChassis(context.Background(), node, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.net.SetStatus(context.Background(), proto.GatewayStatus{Chassis: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if u := read().Underlay; u == nil || u.IP != ip.String() || !u.Registered || u.LastHandshake != nil {
		t.Fatalf("%+v", u)
	}
}

func TestNodeJSONShowsQuarantine(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.enrolledNode(tok, org, "n1")
	read := func() (n struct{ Quarantined bool }) {
		decode(t, e.do("GET", nodesPath(org)+"/"+id, tok), &n)
		return
	}
	if read().Quarantined {
		t.Fatal("quarantined from the start")
	}
	if err := e.net.SetStatus(context.Background(), proto.GatewayStatus{Quarantined: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if !read().Quarantined {
		t.Fatal("quarantine not shown")
	}
}
