package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

// hubVersion goes before newEnv so the fleet's cleanup stops update runs before the version is restored.
func hubVersion(t *testing.T, v string) {
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

func updatesPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/node-updates" }

// onlineNode enrolls a node and keeps it connected with the given version and hypervisor.
func (e *env) onlineNode(by store.User, org uuid.UUID, name, ver string, hv *proto.Hypervisor) store.Node {
	e.t.Helper()
	n, _ := e.onlineNodeConn(by, org, name, ver, hv)
	return n
}

// onlineNodeConn is onlineNode that also returns the node's end of the control stream.
func (e *env) onlineNodeConn(by store.User, org uuid.UUID, name, ver string, hv *proto.Hypervisor) (store.Node, net.Conn) {
	e.t.Helper()
	ctx := context.Background()
	n, token, err := e.store.CreateNode(ctx, store.Node{OrgID: org, Name: name, CreatedBy: by.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	_, cred, err := e.store.EnrollNode(ctx, token)
	if err != nil {
		e.t.Fatal(err)
	}
	_, key, err := wgkey.Generate()
	if err != nil {
		e.t.Fatal(err)
	}
	ip, err := e.store.EnrollChassis(ctx, n.ID, key)
	if err != nil {
		e.t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(e.api.fleet.ServeConnect))
	e.t.Cleanup(srv.Close)
	h := http.Header{"Authorization": {"Bearer " + cred}, proto.HeaderGateway: {gatewayToken}, proto.HeaderUnderlayPeer: {ip.String()}}
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { ws.CloseNow() })
	ctrl := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	reg := proto.Register{NodeID: "x", Hostname: "h", CPUs: 4, MemoryMB: 4096, Version: ver, Hypervisor: hv}
	if err := json.NewEncoder(ctrl).Encode(proto.Message{Type: "register", Register: &reg}); err != nil {
		e.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !e.api.fleet.Online(n.ID) || e.storedVersion(org, n.ID) != ver {
		if time.Now().After(deadline) {
			e.t.Fatal("node did not come online")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return n, ctrl
}

func (e *env) storedVersion(org, id uuid.UUID) string {
	n, err := e.store.Node(context.Background(), org, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return n.Version
}

var fcV = &proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}

func startBody(ids ...uuid.UUID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = `"` + id.String() + `"`
	}
	return `{"nodes":[` + strings.Join(s, ",") + `]}`
}

type runTest struct {
	ID, State, StartedBy string
	EndedAt              *time.Time
	Nodes                []struct{ ID, Name, Status, Reason string }
}

func TestNodeUpdateStartShowCancel(t *testing.T) {
	hubVersion(t, "v0.5.0")
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	a := e.onlineNode(alice, org, "a", "v0.1.0", fcV)
	b := e.onlineNode(alice, org, "b", "v0.1.0", fcV)

	if rec := e.do("GET", updatesPath(org)+"/current", tok); rec.Code != 404 {
		t.Fatalf("current before start: %d", rec.Code)
	}
	rec := e.send("POST", updatesPath(org), tok, startBody(a.ID, b.ID))
	if rec.Code != 201 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var run runTest
	decode(t, rec, &run)
	if run.State != "running" || len(run.Nodes) != 2 || run.Nodes[0].Name != "a" || run.Nodes[1].Name != "b" {
		t.Fatalf("%+v", run)
	}
	if rec := e.send("POST", updatesPath(org), tok, startBody(b.ID)); rec.Code != 409 || !strings.Contains(rec.Body.String(), "an update is already running") {
		t.Fatalf("second start: %d %s", rec.Code, rec.Body.String())
	}
	var cur runTest
	decode(t, e.do("GET", updatesPath(org)+"/current", tok), &cur)
	if cur.ID != run.ID || len(cur.Nodes) != 2 {
		t.Fatalf("%+v", cur)
	}
	if rec := e.do("POST", updatesPath(org)+"/current/cancel", tok); rec.Code != 204 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	var details []string
	for _, a := range e.audit(store.ActionNodeUpdate) {
		if u, ok := a.Details["update"].(string); ok && a.Resource == authz.ObjNodes {
			details = append(details, u)
		}
	}
	slices.Sort(details)
	if !slices.Equal(details, []string{"cancelled", "started"}) {
		t.Fatalf("org-level audits: %v", details)
	}
}

func TestNodeUpdateRejections(t *testing.T) {
	hubVersion(t, "v0.5.0")
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	bob, bobTok := e.user("bob")
	_, carolTok := e.user("carol")
	e.addMember(org, bob, store.RoleMember)
	good := e.onlineNode(alice, org, "good", "v0.1.0", fcV)
	noHV := e.onlineNode(alice, org, "nohv", "v0.1.0", nil)
	current := e.onlineNode(alice, org, "current", "v0.5.0", fcV)
	other := e.createOrg(tok, "Other")
	foreign := e.onlineNode(alice, other, "foreign", "v0.1.0", fcV)

	for name, tc := range map[string]struct {
		tok, body string
		code      int
	}{
		"non-member":     {carolTok, startBody(good.ID), 404},
		"no node:update": {bobTok, startBody(good.ID), 403},
		"no hypervisor":  {tok, startBody(noHV.ID), 409},
		"up to date":     {tok, startBody(current.ID), 409},
		"foreign node":   {tok, startBody(foreign.ID), 404},
		"unknown node":   {tok, startBody(uuid.New()), 404},
		"empty":          {tok, `{"nodes":[]}`, 400},
		"duplicate":      {tok, startBody(good.ID, good.ID), 400},
		"bad id":         {tok, `{"nodes":["x"]}`, 400},
	} {
		if rec := e.send("POST", updatesPath(org), tc.tok, tc.body); rec.Code != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := e.send("POST", updatesPath(org), tok, startBody(noHV.ID)); !strings.Contains(rec.Body.String(), "node nohv cannot be updated") {
		t.Errorf("%s", rec.Body.String())
	}
	if rec := e.do("GET", updatesPath(org)+"/current", carolTok); rec.Code != 404 {
		t.Errorf("non-member current: %d", rec.Code)
	}
	if rec := e.do("POST", updatesPath(org)+"/current/cancel", tok); rec.Code != 404 {
		t.Errorf("cancel without run: %d", rec.Code)
	}
}

func TestNotUpdatableFallback(t *testing.T) {
	e := newEnv(t)
	if got := notUpdatable(e.api.fleet, nil); got != "these nodes cannot be updated" {
		t.Fatalf("%q", got)
	}
}

func TestNodeUpdateCancelNeedsNodeUpdate(t *testing.T) {
	hubVersion(t, "v0.5.0")
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	bob, bobTok := e.user("bob")
	e.addMember(org, bob, store.RoleMember)
	n := e.onlineNode(alice, org, "a", "v0.1.0", fcV)
	if rec := e.send("POST", updatesPath(org), tok, startBody(n.ID)); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	var cur runTest
	decode(t, e.do("GET", updatesPath(org)+"/current", bobTok), &cur)
	if cur.State != "running" {
		t.Fatalf("member cannot see run: %+v", cur)
	}
	if rec := e.do("POST", updatesPath(org)+"/current/cancel", bobTok); rec.Code != 403 {
		t.Fatalf("cancel by member: %d", rec.Code)
	}
}

func TestNodeJSONUpdatable(t *testing.T) {
	hubVersion(t, "v0.5.0")
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	same := e.onlineNode(alice, org, "same", "v0.1.0", fcV)
	older := e.onlineNode(alice, org, "older", "v0.1.0", &proto.Hypervisor{Type: "firecracker", Version: "v1.17.0"})
	for id, want := range map[uuid.UUID]bool{same.ID: true, older.ID: false} {
		var n struct{ Updatable, Updating, UpdatePausesOnly bool }
		decode(t, e.do("GET", nodesPath(org)+"/"+id.String(), tok), &n)
		if !n.Updatable || n.Updating || n.UpdatePausesOnly != want {
			t.Errorf("%s: %+v", id, n)
		}
	}
	if rec := e.send("POST", updatesPath(org), tok, startBody(same.ID)); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	var n struct{ Updatable bool }
	decode(t, e.do("GET", nodesPath(org)+"/"+older.ID.String(), tok), &n)
	if n.Updatable {
		t.Error("updatable during a run")
	}
}
