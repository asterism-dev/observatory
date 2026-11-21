package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

type fakeBuilds map[string]store.ImageBuild

func (f fakeBuilds) BuildBySecret(_ context.Context, secret string) (store.ImageBuild, error) {
	if b, ok := f[secret]; ok {
		return b, nil
	}
	return store.ImageBuild{}, store.ErrNotFound
}

func (e *env) registryHandler(upstream string, builds BuildCredentials) http.Handler {
	e.t.Helper()
	return e.registryHandlerGC(upstream, builds, nil)
}

func (e *env) registryHandlerGC(upstream string, builds BuildCredentials, gc *registry.GC) http.Handler {
	e.t.Helper()
	iss, err := registry.LoadIssuer(e.t.TempDir())
	if err != nil {
		e.t.Fatal(err)
	}
	up, _ := url.Parse(upstream)
	mux := http.NewServeMux()
	RegistryEndpoints(mux, e.store, e.z, Registry{Upstream: up, PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}, Issuer: iss, Builds: builds, GC: gc})
	return mux
}

func (e *env) cliSession(u store.User) string {
	e.t.Helper()
	tok, _, err := e.store.CreateSession(context.Background(), u.ID, store.SessionCLI, time.Hour, "ua", "1.2.3.4")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) registryCredential(cliTok string) string {
	e.t.Helper()
	rec := e.do("POST", "/api/registry/credentials", cliTok)
	if rec.Code != 201 {
		e.t.Fatalf("credential: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ Username, Secret string }
	decode(e.t, rec, &got)
	if got.Username != "observatory" {
		e.t.Fatalf("username %q", got.Username)
	}
	return got.Secret
}

// grant returns the granted actions per repository, or nil with the status when no token was issued.
func grant(t *testing.T, h http.Handler, user, secret string, scopes ...string) (int, map[string][]string) {
	t.Helper()
	q := url.Values{"service": {"observatory"}, "scope": scopes}
	req := httptest.NewRequest("GET", "/v2/token?"+q.Encode(), nil)
	req.SetBasicAuth(user, secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		return rec.Code, nil
	}
	var body struct {
		Token       string
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	decode(t, rec, &body)
	var c registry.Claims
	if _, _, err := jwt.NewParser().ParseUnverified(body.AccessToken, &c); err != nil {
		t.Fatal(err)
	}
	if body.ExpiresIn != 300 || body.Token != body.AccessToken {
		t.Fatalf("expires_in %d", body.ExpiresIn)
	}
	out := map[string][]string{}
	for _, a := range c.Access {
		if a.Type != "repository" {
			t.Fatalf("access %+v", a)
		}
		out[a.Name] = a.Actions
	}
	return rec.Code, out
}

func repoScope(id uuid.UUID, actions string) string {
	return "repository:i/" + id.String() + ":" + actions
}

func TestTokenScopes(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	_, carolTok := e.user("carol")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	other := e.createOrg(carolTok, "Other")
	a := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	b := e.createImage(aliceTok, org, `{"name":"b","scope":"org"}`)
	bobPriv := e.createImage(bobTok, org, `{"name":"mine","scope":"user"}`)
	foreign := e.createImage(carolTok, other, `{"name":"f","scope":"org"}`)
	platform, err := e.store.PlatformImage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	builds := fakeBuilds{"build-a": {ID: uuid.New(), ImageID: a}}
	h := e.registryHandler("http://registry.invalid", builds)
	aliceCred, bobCred := e.registryCredential(e.cliSession(alice)), e.registryCredential(e.cliSession(bob))

	for _, c := range []struct {
		name, user, secret, scope string
		want                      []string
	}{
		{"admin pushes org image", "observatory", aliceCred, repoScope(a, "pull,push"), []string{"pull", "push"}},
		{"member only pulls org image", "observatory", bobCred, repoScope(a, "pull,push"), []string{"pull"}},
		{"owner pushes private image", "observatory", bobCred, repoScope(bobPriv, "pull,push"), []string{"pull", "push"}},
		{"admin cannot pull member's private image", "observatory", aliceCred, repoScope(bobPriv, "pull"), nil},
		{"org X credential for org Y repo", "observatory", aliceCred, repoScope(foreign, "pull"), nil},
		{"builder of A pushes A", "observatory", "build-a", repoScope(a, "pull,push"), []string{"pull", "push"}},
		{"builder of A asks push on B", "observatory", "build-a", repoScope(b, "pull,push"), nil},
		{"builder of A asks other org", "observatory", "build-a", repoScope(foreign, "pull"), nil},
		{"only requested actions", "observatory", aliceCred, repoScope(a, "pull"), []string{"pull"}},
		{"platform image is not in the registry", "observatory", aliceCred, repoScope(platform.ID, "pull"), nil},
		{"not an image repo", "observatory", aliceCred, "repository:library/alpine:pull", nil},
		{"catalog", "observatory", aliceCred, "registry:catalog:*", nil},
	} {
		code, got := grant(t, h, c.user, c.secret, c.scope)
		if code != 200 {
			t.Fatalf("%s: status %d", c.name, code)
		}
		var actions []string
		for _, v := range got {
			actions = v
		}
		if !slices.Equal(actions, c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if code, got := grant(t, h, "observatory", aliceCred); code != 200 || len(got) != 0 {
		t.Fatalf("login without scope: %d %v", code, got)
	}
	for _, c := range []struct{ user, secret string }{{"observatory", "nope"}, {"someone", aliceCred}, {"observatory", ""}} {
		if code, _ := grant(t, h, c.user, c.secret, repoScope(a, "pull")); code != 401 {
			t.Fatalf("%q/%q: status %d", c.user, c.secret, code)
		}
	}
}

func TestTokenOverStorageLimitDropsPush(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	a := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	h := e.registryHandler("http://registry.invalid", fakeBuilds{"build-a": {ID: uuid.New(), ImageID: a}})
	cred := e.registryCredential(e.cliSession(alice))
	one := 1
	if err := e.store.SetLimits(context.Background(), org, store.ScopeOrg, store.Limits{ImageStorageGB: &one}, nil); err != nil {
		t.Fatal(err)
	}
	d := e.addVersion(a, 1)
	e.exec(`INSERT INTO image_version_layers (image_id, version_digest, digest, size) VALUES ($1, $2, 'sha256:l', $3)`, a, d, (1<<30)-1)
	if _, got := grant(t, h, "observatory", cred, repoScope(a, "pull,push")); !slices.Equal(got["i/"+a.String()], []string{"pull", "push"}) {
		t.Fatalf("below limit: %v", got)
	}
	e.exec(`UPDATE image_version_layers SET size = $1`, 1<<30)
	for _, secret := range []string{cred, "build-a"} {
		if _, got := grant(t, h, "observatory", secret, repoScope(a, "pull,push")); !slices.Equal(got["i/"+a.String()], []string{"pull"}) {
			t.Fatalf("%s at limit: %v", secret, got)
		}
	}
}

func TestTokenRefusesPushWhileRegistryClosed(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	a := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	var running atomic.Int32
	running.Store(1)
	gc := registry.NewGC([]string{"true"}, "", func(context.Context) (int, error) { return int(running.Load()), nil }, nil)
	gc.Poll, gc.TokenWait = time.Millisecond, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gc.Run(ctx)
	h := e.registryHandlerGC("http://registry.invalid", fakeBuilds{"build-a": {ID: uuid.New(), ImageID: a}}, gc)
	cred := e.registryCredential(e.cliSession(alice))
	if err := gc.Trigger(); err != nil {
		t.Fatal(err)
	}
	if _, got := grant(t, h, "observatory", cred, repoScope(a, "pull,push")); !slices.Equal(got["i/"+a.String()], []string{"pull", "push"}) {
		t.Fatalf("push while draining: %v", got)
	}
	running.Store(0)
	for gc.Status().State != registry.GCClosed {
		time.Sleep(time.Millisecond)
	}
	for _, secret := range []string{cred, "build-a"} {
		if code, _ := grant(t, h, "observatory", secret, repoScope(a, "pull,push")); code != http.StatusServiceUnavailable {
			t.Fatalf("%s push while closed: %d", secret, code)
		}
	}
	if _, got := grant(t, h, "observatory", cred, repoScope(a, "pull")); !slices.Equal(got["i/"+a.String()], []string{"pull"}) {
		t.Fatalf("pull while closed: %v", got)
	}
}

func TestAdminRegistryGC(t *testing.T) {
	e := newEnv(t)
	admin, adminTok := e.user("root")
	_, userTok := e.user("bob")
	if err := e.store.SetPlatformAdmin(context.Background(), admin.ID, true); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("GET", "/api/admin/registry-gc", adminTok); rec.Code != 404 {
		t.Fatalf("without gc: %d", rec.Code)
	}
	gc := registry.NewGC([]string{"true"}, "04:00", func(context.Context) (int, error) { return 1, nil }, nil)
	e.api.registryGC = gc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gc.Run(ctx)
	if rec := e.do("POST", "/api/admin/registry-gc", userTok); rec.Code != 403 {
		t.Fatalf("non-admin run: %d", rec.Code)
	}
	rec := e.do("GET", "/api/admin/registry-gc", adminTok)
	var st registry.GCStatus
	decode(t, rec, &st)
	if rec.Code != 200 || st.State != registry.GCOpen || st.Schedule != "04:00" || st.Next == nil {
		t.Fatalf("status: %d %+v", rec.Code, st)
	}
	if rec := e.do("POST", "/api/admin/registry-gc", adminTok); rec.Code != 202 {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("POST", "/api/admin/registry-gc", adminTok); rec.Code != 409 {
		t.Fatalf("second run: %d", rec.Code)
	}
}

func TestRegistryCredentialNeedsCLISession(t *testing.T) {
	e := newEnv(t)
	alice, _ := e.user("alice")
	_, sess, err := e.store.CreateSession(t.Context(), alice.ID, store.SessionCLI, time.Hour, "ua", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	// The hub decides from the JWT's sk claim alone, not from the session row.
	for kind, want := range map[store.SessionKind]int{store.SessionBrowser: 403, store.SessionCLI: 201} {
		s := sess
		s.Kind = kind
		jwt, _, err := e.iss.Mint(alice, s)
		if err != nil {
			t.Fatal(err)
		}
		if rec := e.do("POST", "/api/registry/credentials", jwt); rec.Code != want {
			t.Fatalf("%s session: %d %s, want %d", kind, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestCredentialExchangeExpiry(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	a := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	h := e.registryHandler("http://registry.invalid", nil)
	if rec := e.do("POST", "/api/registry/credentials", aliceTok); rec.Code != 403 {
		t.Fatalf("browser session: %d", rec.Code)
	}
	cli := e.cliSession(alice)
	rec := e.do("POST", "/api/registry/credentials", cli)
	var got struct {
		Secret    string
		ExpiresAt time.Time
	}
	decode(t, rec, &got)
	if d := time.Until(got.ExpiresAt); d > 12*time.Hour+5*time.Second || d < 12*time.Hour-time.Minute {
		t.Fatalf("expiresAt in %v", d)
	}
	var stored int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM registry_credentials WHERE secret_hash = sha256($1::bytea)`, []byte(got.Secret)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored hashed: %d %v", stored, err)
	}
	var bound bool
	if err := e.pool.QueryRow(t.Context(), `SELECT bool_and(c.user_id = s.user_id AND abs(extract(epoch FROM c.session_expires_at - s.expires_at)) < 1)
		FROM registry_credentials c JOIN sessions s ON s.id = c.session_id`).Scan(&bound); err != nil || !bound {
		t.Fatalf("credential bound to the session's user and expiry: %v %v", bound, err)
	}
	if code, _ := grant(t, h, "observatory", got.Secret, repoScope(a, "pull")); code != 200 {
		t.Fatalf("fresh credential: %d", code)
	}
	e.exec(`UPDATE registry_credentials SET expires_at = now() - interval '1 second'`)
	if code, _ := grant(t, h, "observatory", got.Secret, repoScope(a, "pull")); code != 401 {
		t.Fatalf("expired credential: %d", code)
	}
	ended := e.registryCredential(cli)
	e.exec(`UPDATE registry_credentials SET session_expires_at = now() - interval '1 second'`)
	if code, _ := grant(t, h, "observatory", ended, repoScope(a, "pull")); code != 401 {
		t.Fatalf("credential of an expired session: %d", code)
	}
}

func TestRegistryCredentialRefusedAfterSessionRevokeEvent(t *testing.T) {
	e := newEnv(t)
	alice, _ := e.user("alice")
	cli := e.cliSession(alice)
	var session uuid.UUID
	if err := e.pool.QueryRow(t.Context(), "SELECT id FROM sessions WHERE user_id = $1 AND kind = 'cli'", alice.ID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if err := e.store.DeleteSessionCredentials(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("POST", "/api/registry/credentials", cli); rec.Code != 401 {
		t.Fatalf("credential for a revoked session: %d %s", rec.Code, rec.Body.String())
	}
}

// No consumer runs here, so the refusal must come from the revoke itself, not from the bus.
func TestRegistryCredentialRefusedAtSessionRevoke(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	a := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	h := e.registryHandler("http://registry.invalid", nil)
	cli := e.cliSession(alice)
	cred := e.registryCredential(cli)
	sess, err := e.store.SessionByToken(t.Context(), cli)
	if err != nil {
		t.Fatal(err)
	}
	// The edge may still hold a JWT minted before the revoke.
	jwt, _, err := e.iss.Mint(alice, sess)
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.do("DELETE", "/api/me/sessions/"+sess.ID.String(), aliceTok); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if code, _ := grant(t, h, "observatory", cred, repoScope(a, "pull")); code != 401 {
		t.Fatalf("credential of a revoked session: %d", code)
	}
	if rec := e.do("POST", "/api/registry/credentials", jwt); rec.Code != 401 {
		t.Fatalf("new credential for a revoked session: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyStreamsBlob(t *testing.T) {
	e := newEnv(t)
	const size = 1 << 30
	firstByte := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://registry:5000/token",service="observatory"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/v2/i/x/blobs/sha256:big":
			pr, pw := io.Pipe()
			go func() {
				_, _ = pw.Write(make([]byte, 1<<20))
				select {
				case <-firstByte:
				case <-time.After(10 * time.Second):
					pw.CloseWithError(errors.New("proxy buffered the response"))
					return
				}
				time.Sleep(300 * time.Millisecond) // longer than the hub's write timeout below
				_, err := io.CopyN(pw, zeroReader{}, size-1<<20)
				pw.CloseWithError(err)
			}()
			w.Header().Set("Content-Length", "1073741824")
			_, _ = io.Copy(w, pr)
		case "/v2/i/x/blobs/uploads/u1":
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil || n != 16<<20 {
				t.Errorf("upload: %d %v", n, err)
			}
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer upstream.Close()
	hub := httptest.NewUnstartedServer(e.registryHandler(upstream.URL, nil))
	hub.Config.WriteTimeout, hub.Config.ReadTimeout = 100*time.Millisecond, 100*time.Millisecond
	hub.Start()
	defer hub.Close()

	resp, err := http.Get(hub.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="https://hub.test/v2/token",service="observatory"` {
		t.Fatalf("realm: %q", got)
	}

	resp, err = http.Get(hub.URL + "/v2/i/x/blobs/sha256:big")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	close(firstByte)
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n+1 != size {
		t.Fatalf("blob: %d %v", n+1, err)
	}

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(make([]byte, 8<<20))
		time.Sleep(300 * time.Millisecond) // longer than the hub's read timeout
		_, _ = pw.Write(make([]byte, 8<<20))
		pw.Close()
	}()
	req, _ := http.NewRequest("PUT", hub.URL+"/v2/i/x/blobs/uploads/u1", pr)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestProxyRealmFollowsConfiguredHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://registry:5000/token",service="observatory"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	node := &url.URL{Scheme: "http", Host: "100.64.0.1:8090"}
	h := registryProxy(up, &url.URL{Scheme: "https", Host: "hub.test"}, []*url.URL{node})
	for host, want := range map[string]string{
		"100.64.0.1:8090": "http://100.64.0.1:8090/v2/token",
		"hub.test":        "https://hub.test/v2/token",
		"evil.test":       "https://hub.test/v2/token",
	} {
		r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="`+want+`",service="observatory"` {
			t.Errorf("host %s: %q", host, got)
		}
	}
}

func TestProxyStripsEdgeHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Clone() }))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	r.Header.Set(clientip.HeaderClientIP, "203.0.113.7")
	r.Header.Set(clientip.HeaderEdge, "0123456789abcdef0123456789abcdef")
	registryProxy(up, &url.URL{Scheme: "https", Host: "hub.test"}, nil).ServeHTTP(httptest.NewRecorder(), r)
	h := <-got
	for _, k := range []string{clientip.HeaderClientIP, clientip.HeaderEdge} {
		if v := h.Values(k); len(v) != 0 {
			t.Errorf("registry saw %s: %q", k, v)
		}
	}
}
