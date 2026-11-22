package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignerMintVerifies(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(key, testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/internal/iam/jwks", s.JWKSHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	v, err := NewVerifier(srv.URL, testIssuer+"/", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}

	sessionEnd := time.Now().Add(time.Minute)
	want := Principal{User: uuid.New(), Session: uuid.New(), PlatformAdmin: true, Name: "Alice\x07 Doe", SessionKind: "cli", SessionExpires: sessionEnd}
	raw, exp, err := s.Mint(want)
	if err != nil {
		t.Fatal(err)
	}
	// A token never outlives its session.
	if exp.After(sessionEnd) || sessionEnd.Sub(exp) >= time.Second {
		t.Fatalf("exp %v, session ends %v", exp, sessionEnd)
	}
	got, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	want.Name = "Alice Doe"
	want.SessionExpires = time.Unix(sessionEnd.Unix(), 0)
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	long := Principal{User: uuid.New(), Session: uuid.New(), SessionExpires: time.Now().Add(time.Hour)}
	if _, exp, err := s.Mint(long); err != nil || time.Until(exp) > 5*time.Minute {
		t.Fatalf("exp %v err %v, want at most 5 min", exp, err)
	}
}

func TestNewSignerRejectsBadKey(t *testing.T) {
	if _, err := NewSigner(ed25519.PrivateKey{1, 2, 3}, testIssuer); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestSignerVerifierNeedsNoJWKS(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(key, testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{User: uuid.New(), Session: uuid.New(), SessionKind: "cli", SessionExpires: time.Now().Add(time.Hour)}
	raw, _, err := s.Mint(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Verifier().Verify(context.Background(), raw); err != nil || got.User != p.User {
		t.Fatalf("verify own token: %+v %v", got, err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os, err := NewSigner(other, testIssuer)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := os.Mint(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verifier().Verify(context.Background(), foreign); err == nil {
		t.Fatal("foreign key accepted")
	}
}
