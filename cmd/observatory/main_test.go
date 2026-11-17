package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialsRoundTripPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observatory", "credentials.json")
	want := credentials{Hub: "http://hub.test", Token: "tok"}
	if err := saveCredentials(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", di.Mode().Perm())
	}
}

func TestWhoami(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"user":{"id":"u1","email":"ada@example.com","name":"Ada Lovelace","platformAdmin":false},"orgs":[{"id":"o1","name":"Ada","personal":true,"role":"owner"}]}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := whoami(context.Background(), credentials{Hub: srv.URL, Token: "good"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Ada Lovelace", "ada@example.com", "Ada (owner)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output %q missing %q", out.String(), s)
		}
	}

	err := whoami(context.Background(), credentials{Hub: srv.URL, Token: "bad"}, &out)
	if err == nil || !strings.Contains(err.Error(), "observatory login") {
		t.Fatalf("want login hint error, got %v", err)
	}
}
