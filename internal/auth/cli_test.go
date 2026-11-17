package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) cliToken(body string) *httptest.ResponseRecorder {
	return e.do(httptest.NewRequest("POST", "/api/auth/cli-token", strings.NewReader(body)))
}

func TestCLITokenExchange(t *testing.T) {
	e := newEnv(t)
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

	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	if rec := e.do(req); rec.Code != http.StatusOK || rec.Body.String() != "a@x.io" {
		t.Fatalf("bearer: %d %q", rec.Code, rec.Body)
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
	e := newEnv(t)
	rec := e.cliToken(`{"idToken":"bad"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	if !e.actions(store.ResultFailed)[store.ActionAuthCLILogin] {
		t.Fatal("no failed audit")
	}
	entries, err := e.store.AuditEntries(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if d, _ := json.Marshal(en.Details); strings.Contains(string(d), "secret-detail") || strings.Contains(string(d), "bad") {
			t.Fatalf("audit leaks verifier error: %s", d)
		}
	}
	if len(e.actions(store.ResultAllowed)) != 0 {
		t.Fatal("unexpected allowed audit")
	}
}

func TestCLITokenRejectsMalformed(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{``, `nope`, `{}`, `{"idToken":""}`, `{"idToken":"` + strings.Repeat("a", 70<<10) + `"}`} {
		if rec := e.cliToken(b); rec.Code != http.StatusBadRequest {
			t.Errorf("body len %d: status %d", len(b), rec.Code)
		}
	}
}

func TestAuthConfig(t *testing.T) {
	e := newEnv(t)
	rec := e.do(httptest.NewRequest("GET", "/api/auth/config", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"cliClientId":"cli-client","issuer":"https://idp.test"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
