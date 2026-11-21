package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
	"github.com/asterism-dev/observatory/internal/wgkey"
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
	NodeEndpoints(mux, e.store, e.newFleet(), e.net)
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

func TestNodeJSONImage(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(tok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	id := e.enrolledNode(tok, org, "n1")
	read := func() (n struct{ Image *proto.ImageStatus }) {
		decode(t, e.do("GET", nodesPath(org)+"/"+id, bobTok), &n)
		return
	}
	if read().Image != nil {
		t.Fatal("image before report")
	}
	node, _ := uuid.Parse(id)
	if err := e.store.SetNodeImage(context.Background(), node, proto.ImageStatus{Version: "v1", State: proto.ImagePulling}); err != nil {
		t.Fatal(err)
	}
	if i := read().Image; i == nil || i.State != proto.ImagePulling || i.Version != "v1" {
		t.Fatalf("%+v", i)
	}
}

func TestNodeJSONHypervisor(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.enrolledNode(tok, org, "n1")
	read := func() (n struct{ Hypervisor *proto.Hypervisor }) {
		decode(t, e.do("GET", nodesPath(org)+"/"+id, tok), &n)
		return
	}
	if read().Hypervisor != nil {
		t.Fatal("hypervisor before report")
	}
	node, _ := uuid.Parse(id)
	if err := e.store.UpdateNodeInfo(context.Background(), node, 2, 2048, nil, "", &proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}); err != nil {
		t.Fatal(err)
	}
	if h := read().Hypervisor; h == nil || h.Version != "v1.18.0" {
		t.Fatalf("%+v", h)
	}
}

func TestNodeJSONOutdated(t *testing.T) {
	old := version.Version
	version.Version = "v0.3.0"
	t.Cleanup(func() { version.Version = old })
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.enrolledNode(tok, org, "n1")
	node, _ := uuid.Parse(id)
	for v, want := range map[string]bool{"v0.2.0": true, "v0.3.0": false, "": false, "dev": false} {
		if err := e.store.UpdateNodeInfo(context.Background(), node, 2, 2048, nil, v, nil); err != nil {
			t.Fatal(err)
		}
		var n struct{ Outdated bool }
		decode(t, e.do("GET", nodesPath(org)+"/"+id, tok), &n)
		if n.Outdated != want {
			t.Errorf("node version %q: outdated %v, want %v", v, n.Outdated, want)
		}
	}
}

func chassisCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func (e *env) underlay(credential, key, csr string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(proto.UnderlayRequest{ChassisKey: key, ChassisCSR: csr})
	mux := http.NewServeMux()
	NodeEndpoints(mux, e.store, e.newFleet(), e.net)
	req := httptest.NewRequest("POST", "/api/nodes/underlay", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+credential)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) credentialFor(tok string, org uuid.UUID, name string) (id, credential string) {
	e.t.Helper()
	c := e.createNode(tok, org, name)
	rec := e.enroll(tokenOf(c.InstallCommand))
	var got struct{ Credential string }
	decode(e.t, rec, &got)
	return c.Node.ID, got.Credential
}

func TestNodeUnderlayEndpoint(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	_, cred := e.credentialFor(tok, org, "n1")
	_, key, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var first, second proto.Underlay
	rec := e.underlay(cred, key, chassisCSR(t))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	decode(t, rec, &first)
	if !netaddr.UnderlayPool.Contains(netip.MustParsePrefix(first.Address).Addr()) || first.SBRemote != "ssl:10.198.0.1:6642" || first.Cert == "" || first.CACert == "" {
		t.Fatalf("%+v", first)
	}
	decode(t, e.underlay(cred, key, chassisCSR(t)), &second)
	if second.Address != first.Address {
		t.Fatalf("address changed: %s -> %s", first.Address, second.Address)
	}
}

func TestNodeUnderlayEndpointAllowsQuarantined(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id, cred := e.credentialFor(tok, org, "n1")
	node, _ := uuid.Parse(id)
	if _, err := e.store.QuarantineNodes(context.Background(), []uuid.UUID{node}); err != nil {
		t.Fatal(err)
	}
	_, key, _ := wgkey.Generate()
	if rec := e.underlay(cred, key, chassisCSR(t)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestNodeUnderlayEndpointRejectsBadCredential(t *testing.T) {
	e := newEnv(t)
	_, key, _ := wgkey.Generate()
	if rec := e.underlay(uuid.NewString()+".wrong", key, chassisCSR(t)); rec.Code != 401 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestNodeUnderlayEndpointRejectsInvalidInput(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	_, cred := e.credentialFor(tok, org, "n1")
	_, key, _ := wgkey.Generate()
	if rec := e.underlay(cred, "bogus", chassisCSR(t)); rec.Code != 400 {
		t.Fatalf("bad key: %d", rec.Code)
	}
	if rec := e.underlay(cred, key, "not a csr"); rec.Code != 400 {
		t.Fatalf("bad csr: %d", rec.Code)
	}
}

func TestNodeUnderlayEndpointNoNetwork(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	_, cred := e.credentialFor(tok, org, "n1")
	mux := http.NewServeMux()
	NodeEndpoints(mux, e.store, e.newFleet(), nil)
	call := func(c string) int {
		req := httptest.NewRequest("POST", "/api/nodes/underlay", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+c)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := call("bad"); got != 401 {
		t.Fatalf("unauthenticated: %d", got)
	}
	if got := call(cred); got != 503 {
		t.Fatalf("authenticated: %d", got)
	}
}

func TestHealthEndpoint(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	_, cred := e.credentialFor(tok, org, "n1")
	_, key, _ := wgkey.Generate()
	var u proto.Underlay
	decode(t, e.underlay(cred, key, chassisCSR(t)), &u)
	peer := netip.MustParsePrefix(u.Address).Addr().String()
	mux := http.NewServeMux()
	NodeEndpoints(mux, e.store, e.newFleet(), e.net)
	call := func(h map[string]string) int {
		req := httptest.NewRequest("GET", "/api/nodes/health", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	auth := "Bearer " + cred
	if got := call(map[string]string{"Authorization": auth, proto.HeaderGateway: gatewayToken, proto.HeaderUnderlayPeer: peer}); got != 204 {
		t.Fatalf("via gateway: %d", got)
	}
	if got := call(map[string]string{"Authorization": auth, proto.HeaderUnderlayPeer: peer}); got != 401 {
		t.Fatalf("direct: %d", got)
	}
	if got := call(map[string]string{"Authorization": auth, proto.HeaderGateway: gatewayToken, proto.HeaderUnderlayPeer: "10.198.255.250"}); got != 403 {
		t.Fatalf("other peer: %d", got)
	}
}

func (e *env) emergency(credential, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	NodeEndpoints(mux, e.store, e.api.fleet, e.net)
	req := httptest.NewRequest("POST", "/api/nodes/emergency", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+credential)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestEmergencyEndpointShowsUnderlayDown(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id, cred := e.credentialFor(tok, org, "n1")
	node, _ := uuid.Parse(id)
	if _, err := e.store.QuarantineNodes(context.Background(), []uuid.UUID{node}); err != nil {
		t.Fatal(err)
	}
	rec := e.emergency(cred, `{"version":"v0.1.0","reason":"health check failed"}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"update":false}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var n struct{ State string }
	decode(t, e.do("GET", nodesPath(org)+"/"+id, tok), &n)
	if n.State != "underlay_down" {
		t.Fatalf("state %q", n.State)
	}
}

func TestEmergencyEndpointRejects(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	_, cred := e.credentialFor(tok, org, "n1")
	if rec := e.emergency(uuid.NewString()+".wrong", `{"version":"v0.1.0"}`); rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != "Unauthorized" {
		t.Fatalf("bad credential: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.emergency(cred, `{"version":"v0.1.0","update":true}`); rec.Code != 400 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	if rec := e.emergency(cred, `{"reason":"`+strings.Repeat("x", 20<<10)+`"}`); rec.Code != 400 {
		t.Fatalf("oversized body: %d", rec.Code)
	}
	if rec := e.emergency(cred, `{"version":"v`+strings.Repeat("1", 100)+`"}`); rec.Code != 400 {
		t.Fatalf("overlong version: %d", rec.Code)
	}
}

func TestEmergencyEndpointStoresVersion(t *testing.T) {
	hubVersion(t, "v0.5.0")
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id, cred := e.credentialFor(tok, org, "n1")
	node, _ := uuid.Parse(id)
	if err := e.store.UpdateNodeInfo(context.Background(), node, 4, 4096, nil, "v0.4.0", fcV); err != nil {
		t.Fatal(err)
	}
	read := func() (n struct {
		Version             string
		Outdated, Updatable bool
	}) {
		decode(t, e.do("GET", nodesPath(org)+"/"+id, tok), &n)
		return
	}
	e.emergency(cred, `{"version":"v0.4.0"}`)
	if n := read(); n.Version != "v0.4.0" || !n.Updatable {
		t.Fatalf("before: %+v", n)
	}
	e.emergency(cred, `{"version":"v0.5.0"}`)
	if n := read(); n.Version != "v0.5.0" || n.Outdated || n.Updatable {
		t.Fatalf("after: %+v", n)
	}
}
