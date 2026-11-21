package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

func devicesPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/devices" }

type deviceTest struct {
	ID, Name, IP string
	Owner        struct{ ID, Name string }
	CanDelete    bool
}

func (e *env) createDevice(tok string, org uuid.UUID, name string) (deviceTest, string) {
	e.t.Helper()
	rec := e.send("POST", devicesPath(org), tok, `{"name":"`+name+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("create device: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Device deviceTest
		Config string
	}
	decode(e.t, rec, &got)
	return got.Device, got.Config
}

func (e *env) listDevices(tok string, org uuid.UUID) []deviceTest {
	e.t.Helper()
	var out []deviceTest
	decode(e.t, e.do("GET", devicesPath(org), tok), &out)
	return out
}

func TestDeviceCreateConfigOnce(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")

	rec := e.send("POST", devicesPath(org), tok, `{"name":"laptop"}`)
	if rec.Code != 201 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %v", rec.Code, rec.Header())
	}
	var got struct {
		Device deviceTest
		Config string
	}
	decode(t, rec, &got)
	slice, err := e.store.OrgSlice(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{}
	for _, l := range strings.Split(got.Config, "\n") {
		if k, v, ok := strings.Cut(l, " = "); ok {
			cfg[k] = v
		}
	}
	priv := cfg["PrivateKey"]
	want := map[string]string{
		"Address": got.Device.IP + "/32", "DNS": "10.192.0.1", "MTU": "1300", "PublicKey": e.net.ClientsPublicKey(),
		"Endpoint": "hub.test:51821", "AllowedIPs": slice.String() + ", 10.192.0.1/32", "PersistentKeepalive": "25",
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Fatalf("%s = %q, want %q\n%s", k, cfg[k], v, got.Config)
		}
	}
	if !strings.HasPrefix(got.Config, "[Interface]\nPrivateKey = ") || !strings.Contains(got.Config, "\n\n[Peer]\n") {
		t.Fatalf("layout:\n%s", got.Config)
	}
	pub, err := wgkey.Public(priv)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := e.store.Devices(context.Background(), org)
	if err != nil || len(ds) != 1 || ds[0].PublicKey != pub {
		t.Fatalf("stored: %+v %v", ds, err)
	}

	leaks := []string{e.do("GET", devicesPath(org), tok).Body.String()}
	for _, a := range []string{store.ActionDeviceCreate, store.ActionDeviceDelete} {
		for _, en := range e.audit(a) {
			b, _ := json.Marshal(en)
			leaks = append(leaks, string(b))
		}
	}
	var row string
	if err := e.pool.QueryRow(context.Background(), "SELECT string_agg(row_to_json(d)::text, '') FROM devices d").Scan(&row); err != nil {
		t.Fatal(err)
	}
	leaks = append(leaks, row)
	for _, l := range leaks {
		if strings.Contains(l, priv) {
			t.Fatalf("private key leaked: %s", l)
		}
	}
	es := e.audit(store.ActionDeviceCreate)
	if len(es) != 1 || es[0].Details["name"] != "laptop" || es[0].Details["ip"] != got.Device.IP {
		t.Fatalf("audit: %+v", es)
	}
}

func TestDeviceVisibility(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	carol, carolTok := e.user("carol")
	dave, daveTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, carol, store.RoleMember)
	e.addMember(org, dave, store.RoleMember)

	cd, _ := e.createDevice(carolTok, org, "carol-phone")
	e.createDevice(daveTok, org, "dave-phone")

	mine := e.listDevices(carolTok, org)
	if len(mine) != 1 || mine[0].ID != cd.ID || mine[0].Owner.Name != "carol" || !mine[0].CanDelete {
		t.Fatalf("member list: %+v", mine)
	}
	all := e.listDevices(adminTok, org)
	if len(all) != 2 || !all[0].CanDelete || !all[1].CanDelete || all[0].Owner.ID == "" {
		t.Fatalf("admin list: %+v", all)
	}

	if rec := e.do("DELETE", devicesPath(org)+"/"+cd.ID, daveTok); rec.Code != 403 {
		t.Fatalf("dave deletes carol's: %d", rec.Code)
	}
	es := e.audit(store.ActionDeviceDelete)
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != dave.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	if rec := e.do("DELETE", devicesPath(org)+"/"+cd.ID, adminTok); rec.Code != 204 {
		t.Fatalf("admin delete: %d", rec.Code)
	}
	if rec := e.do("DELETE", devicesPath(org)+"/"+cd.ID, adminTok); rec.Code != 404 {
		t.Fatalf("repeat delete: %d", rec.Code)
	}
	if n := len(e.audit(store.ActionDeviceDelete)); n != 2 {
		t.Fatalf("audit rows: %d", n)
	}
}

func TestRemoveMemberDeletesDevicesAPI(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.createDevice(bobTok, org, "phone")
	if rec := e.do("DELETE", memberPath(org, bob), ownerTok); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.listDevices(ownerTok, org); len(got) != 0 {
		t.Fatalf("devices remain: %+v", got)
	}
}

type loginAuth struct{}

func (loginAuth) AuthCodeURL(string, string, bool) string { return "" }

func (loginAuth) Exchange(context.Context, string, string) (store.Identity, error) {
	return store.Identity{}, nil
}

func (loginAuth) VerifyCLIToken(context.Context, string, string) (store.Identity, error) {
	return store.Identity{Subject: "fresh", Email: "fresh@x.io", Name: "fresh"}, nil
}

func TestFreshLoginCanCreateDevice(t *testing.T) {
	e := newEnv(t)
	a := &auth.Handler{Store: e.store, Auth: loginAuth{}, Reload: e.z.Reload}
	mux := http.NewServeMux()
	a.Routes(mux)
	req := httptest.NewRequest("POST", "/api/auth/cli-token", strings.NewReader(`{"idToken":"x","accessToken":"y"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var tok struct{ Token string }
	decode(t, rec, &tok)
	var me struct{ User struct{ ID uuid.UUID } }
	decode(t, e.do("GET", "/api/me", tok.Token), &me)
	e.createDevice(tok.Token, e.personalOrg(store.User{ID: me.User.ID}), "laptop")
}

func TestLoginReloadIgnoresRequestCancel(t *testing.T) {
	e := newEnv(t)
	called := false
	reload := func(ctx context.Context) error {
		called = true
		if ctx.Done() != nil {
			t.Error("reload runs on a cancellable request context")
		}
		return e.z.Reload(ctx)
	}
	a := &auth.Handler{Store: e.store, Auth: loginAuth{}, Reload: reload}
	mux := http.NewServeMux()
	a.Routes(mux)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/api/auth/cli-token", strings.NewReader(`{"idToken":"x","accessToken":"y"}`)).WithContext(ctx)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("reload not called")
	}
}

func TestDeviceDuplicateName(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.createDevice(tok, org, "laptop")
	if rec := e.send("POST", devicesPath(org), tok, `{"name":"laptop"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", devicesPath(org), tok, `{"name":""}`); rec.Code != 400 {
		t.Fatalf("empty: %d", rec.Code)
	}
}

func TestDeviceConfigUsesGuestMTU(t *testing.T) {
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	if err := e.store.SetGuestMTU(t.Context(), 1250, alice.ID); err != nil {
		t.Fatal(err)
	}
	_, config := e.createDevice(tok, org, "laptop")
	if !strings.Contains(config, "\nMTU = 1250\n") {
		t.Fatalf("config:\n%s", config)
	}
}
