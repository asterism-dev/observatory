package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
	"github.com/asterism-dev/observatory/internal/platform/identity"
)

const edgeToken = "0123456789abcdef0123456789abcdef"

type env struct {
	t      *testing.T
	iam    *iamtest.Server
	signer *identity.Signer
	a      *Handler
	alice  iamtest.User
}

func newEnv(t *testing.T) *env {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, iamtest.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	iam := iamtest.New(t)
	return &env{t: t, iam: iam, signer: signer, a: &Handler{Verifier: signer.Verifier(), EdgeToken: edgeToken, IAM: iam.Client()}, alice: iam.AddUser("Alice")}
}

// mint signs p for a fresh iam session of Alice's and returns the JWT and the session.
func (e *env) mint(edit func(*identity.Principal)) (string, uuid.UUID) {
	e.t.Helper()
	_, sess := e.iam.SessionToken(e.alice.ID, SessionCLI)
	p := identity.Principal{User: e.alice.ID, Session: sess, PlatformAdmin: true, Name: e.alice.Name, SessionKind: SessionCLI, SessionExpires: time.Now().Add(time.Hour)}
	if edit != nil {
		edit(&p)
	}
	tok, _, err := e.signer.Mint(p)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, sess
}

func (e *env) whoami(set func(*http.Request)) (*httptest.ResponseRecorder, Principal) {
	var got Principal
	req := httptest.NewRequest("GET", "/whoami", nil)
	set(req)
	rec := httptest.NewRecorder()
	e.a.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = PrincipalFrom(r.Context()) })).ServeHTTP(rec, req)
	return rec, got
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func TestRequirePrincipalFromClaims(t *testing.T) {
	e := newEnv(t)
	tok, sess := e.mint(nil)
	rec, got := e.whoami(bearer(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got.User != (User{ID: e.alice.ID, Name: "Alice", PlatformAdmin: true}) || got.Session.ID != sess || got.Session.Kind != SessionCLI || time.Until(got.Session.ExpiresAt) < 59*time.Minute {
		t.Fatalf("principal %+v", got)
	}
	// The JWT is still valid, but its session is not.
	e.iam.RevokeSession(sess)
	if rec, _ := e.whoami(bearer(tok)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", rec.Code)
	}
}

func TestRequireRejects(t *testing.T) {
	e := newEnv(t)
	tok, _ := e.mint(nil)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forger, err := identity.NewSigner(other, iamtest.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	forged, _, err := forger.Mint(identity.Principal{User: e.alice.ID, Session: uuid.New(), SessionKind: SessionCLI, SessionExpires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	noKind, _ := e.mint(func(p *identity.Principal) { p.SessionKind = "" })
	noExpiry, _ := e.mint(func(p *identity.Principal) { p.SessionExpires = time.Time{} })
	unknownSession, _, err := e.signer.Mint(identity.Principal{User: e.alice.ID, Session: uuid.New(), SessionKind: SessionCLI, SessionExpires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]func(*http.Request){
		"none":            func(*http.Request) {},
		"jwt cookie":      func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "obs_session", Value: tok}) },
		"basic":           func(r *http.Request) { r.Header.Set("Authorization", "Basic "+tok) },
		"forged":          bearer(forged),
		"no sk":           bearer(noKind),
		"no sx":           bearer(noExpiry),
		"unknown session": bearer(unknownSession),
	} {
		if rec, _ := e.whoami(set); rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"error":"unauthorized"}`+"\n" {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestRequireFailsClosedWhenIAMUnreachable(t *testing.T) {
	e := newEnv(t)
	tok, _ := e.mint(nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	e.a.IAM = iamv1connect.NewIAMInternalServiceClient(http.DefaultClient, dead.URL+"/internal/iam")
	called := false
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	e.a.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(rec, req.WithContext(context.Background()))
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("status %d, handler called %v", rec.Code, called)
	}
}

func TestRequestMetaUsesEdgeHeader(t *testing.T) {
	h := &Handler{EdgeToken: edgeToken}
	for token, want := range map[string]string{edgeToken: "203.0.113.7", "wrong": "192.0.2.1", "": "192.0.2.1"} {
		var got string
		h.TrustEdge(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = RequestMeta(r) })).
			ServeHTTP(httptest.NewRecorder(), func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set(clientip.HeaderClientIP, "203.0.113.7")
				r.Header.Set(clientip.HeaderEdge, token)
				return r
			}())
		if got != want {
			t.Errorf("edge token %q: ip %q, want %q", token, got, want)
		}
	}
}
