package iam

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/iam/store/storetest"
)

func (e *loginEnv) cliToken(body string) *httptest.ResponseRecorder {
	return e.do(httptest.NewRequest("POST", "/api/auth/cli-token", strings.NewReader(body)))
}

func TestCLITokenExchange(t *testing.T) {
	e := newLoginEnv(t)
	rec := e.cliToken(`{"idToken":"good","accessToken":"at"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.Token, "obs_") {
		t.Fatalf("bad body %s: %v", rec.Body, err)
	}
	if d := time.Until(out.ExpiresAt); d < 29*24*time.Hour || d > 30*24*time.Hour {
		t.Fatalf("expiry %v", d)
	}

	sess, err := e.store.SessionByToken(context.Background(), out.Token)
	if err != nil || sess.Kind != store.SessionCLI {
		t.Fatalf("session %+v, %v", sess, err)
	}
	got := e.actions(store.ResultAllowed)
	for _, a := range []string{store.ActionAuthCLILogin, store.ActionOrgCreate, store.ActionPlatformGrantAdmin} {
		if !got[a] {
			t.Errorf("missing audit %s", a)
		}
	}
	if got[store.ActionAuthLogin] {
		t.Error("cli login audited as browser login")
	}
}

func TestCLITokenRejectsInvalid(t *testing.T) {
	e := newLoginEnv(t)
	rec := e.cliToken(`{"idToken":"bad"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	if !e.actions(store.ResultFailed)[store.ActionAuthCLILogin] {
		t.Fatal("no failed audit")
	}
	for _, en := range storetest.Audit(t, e.pool, "") {
		if d, _ := json.Marshal(en.Details); strings.Contains(string(d), "secret-detail") || strings.Contains(string(d), "bad") {
			t.Fatalf("audit leaks verifier error: %s", d)
		}
	}
	if len(e.actions(store.ResultAllowed)) != 0 {
		t.Fatal("unexpected allowed audit")
	}
}

func TestCLITokenRejectsMalformed(t *testing.T) {
	e := newLoginEnv(t)
	for _, b := range []string{``, `nope`, `{}`, `{"idToken":""}`, `{"idToken":"` + strings.Repeat("a", 70<<10) + `"}`} {
		if rec := e.cliToken(b); rec.Code != http.StatusBadRequest {
			t.Errorf("body len %d: status %d", len(b), rec.Code)
		}
	}
}

func TestAuthConfig(t *testing.T) {
	e := newLoginEnv(t)
	rec := e.do(httptest.NewRequest("GET", "/api/auth/config", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"cliClientId":"cli-client","issuer":"https://idp.test"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestCheckAZP(t *testing.T) {
	for _, c := range []struct {
		aud     []string
		azp     string
		wantErr bool
	}{
		{[]string{"cli"}, "", false},
		{[]string{"cli"}, "cli", false},
		{[]string{"cli", "web", "project"}, "cli", false},
		{[]string{"cli"}, "web", true},
		{[]string{"cli", "web", "project"}, "web", true},
		{[]string{"cli", "web", "project"}, "", true},
	} {
		if err := checkAZP(c.aud, c.azp, "cli"); (err != nil) != c.wantErr {
			t.Errorf("aud %v azp %q: err %v", c.aud, c.azp, err)
		}
	}
}

func TestLoginReloadIgnoresRequestCancel(t *testing.T) {
	e := newLoginEnv(t)
	called := false
	e.a.Reload = func(ctx context.Context, _, _ uuid.UUID) error {
		called = true
		if ctx.Done() != nil {
			t.Error("reload runs on a cancellable request context")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if rec := e.do(httptest.NewRequest("POST", "/api/auth/cli-token", strings.NewReader(`{"idToken":"good","accessToken":"at"}`)).WithContext(ctx)); rec.Code != http.StatusCreated {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if !called {
		t.Fatal("reload not called")
	}
}

// slowHub answers CanHold only after delay, like a hub still behind the login's revision.
type slowHub struct {
	hubv1connect.ResourceServiceClient
	delay time.Duration
	got   chan *hubv1.CanHoldRequest
}

func (h slowHub) CanHold(ctx context.Context, req *connect.Request[hubv1.CanHoldRequest]) (*connect.Response[hubv1.CanHoldResponse], error) {
	h.got <- req.Msg
	select {
	case <-time.After(h.delay):
		return connect.NewResponse(&hubv1.CanHoldResponse{}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestLoginWaitsForTheHub(t *testing.T) {
	z, err := authz.NewService(t.Context(), func(context.Context) (authz.Policy, error) { return authz.Policy{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	hub := slowHub{delay: 300 * time.Millisecond, got: make(chan *hubv1.CanHoldRequest, 1)}
	user, org := uuid.New(), uuid.New()
	start := time.Now()
	if err := reloadAfterLogin(z, hub, func() uint64 { return 42 })(t.Context(), user, org); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < hub.delay {
		t.Fatalf("login returned after %v, before the hub had the new org", d)
	}
	select {
	case req := <-hub.got:
		if req.MinRevision != 42 || req.OrgId != org.String() || req.Actor != authz.UserSubject(user) || len(req.Rules) != 0 {
			t.Fatalf("CanHold %+v", req)
		}
	default:
		t.Fatal("login never asked the hub")
	}
}
