package buildagent

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestFromRefs(t *testing.T) {
	for _, tc := range []struct {
		name, dockerfile string
		want             []string
	}{
		{"plain", "FROM ubuntu:24.04\nRUN true\n", []string{"ubuntu:24.04"}},
		{"lowercase and comments", "# syntax=docker/dockerfile:1\nfrom alpine\n# FROM commented:out\n", []string{"alpine"}},
		{"arg defaults", "ARG BASE=ubuntu\nARG VER=\"24.04\"\nFROM ${BASE}:$VER\n", []string{"ubuntu:24.04"}},
		{"arg referencing arg", "ARG VER=3.20\nARG IMG=alpine:${VER}\nFROM $IMG\n", []string{"alpine:3.20"}},
		{"arg without default", "ARG TAG\nFROM debian:${TAG:-bookworm}\n", []string{"debian:bookworm"}},
		{"arg after from ignored", "FROM alpine AS a\nARG X=busybox\nFROM $X\n", []string{"alpine"}},
		{"stage names", "FROM golang:1.23 AS Build\nRUN go build\nFROM build AS test\nFROM gcr.io/distroless/static\nCOPY --from=build /x /x\n",
			[]string{"golang:1.23", "gcr.io/distroless/static"}},
		{"platform flag", "FROM --platform=$BUILDPLATFORM golang:1.23 AS b\nFROM --platform=linux/amd64 alpine\n", []string{"golang:1.23", "alpine"}},
		{"platform args", "FROM example.org/tools:${TARGETARCH}\n", []string{"example.org/tools:amd64"}},
		{"scratch", "FROM scratch\nCOPY x /\n", nil},
		{"digest pinned", "FROM ubuntu@sha256:" + strings.Repeat("a", 64) + "\nFROM alpine\n", []string{"alpine"}},
		{"duplicates", "FROM alpine AS a\nFROM alpine AS b\n", []string{"alpine"}},
		{"hub image", "FROM hub.example.com/i/0b9c4c55-1d1e-4a35-8e0c-3f1f0bb7a1aa:latest\n", []string{"hub.example.com/i/0b9c4c55-1d1e-4a35-8e0c-3f1f0bb7a1aa:latest"}},
		{"continuation", "FROM \\\n  # note\n  alpine \\\n  AS base\nFROM base\n", []string{"alpine"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromRefs(tc.dockerfile); !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// basicAuth makes a registry answer only requests with the build credential, as the hub's token endpoint does.
func basicAuth(h http.Handler, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "observatory" || p != secret {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func TestResolveBases(t *testing.T) {
	quiet := ggcrregistry.Logger(log.New(io.Discard, "", 0))
	public := httptest.NewServer(ggcrregistry.New(quiet))
	defer public.Close()
	hubSrv := httptest.NewServer(basicAuth(ggcrregistry.New(quiet), "build-secret-1"))
	defer hubSrv.Close()
	pub, hubHost := strings.TrimPrefix(public.URL, "http://"), strings.TrimPrefix(hubSrv.URL, "http://")

	amd, _ := random.Image(64, 1)
	arm, _ := random.Image(64, 1)
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
		mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	if err := remote.WriteIndex(mustRef(t, pub+"/library/ubuntu:24.04"), idx); err != nil {
		t.Fatal(err)
	}
	own, _ := random.Image(64, 1)
	ownRef := hubHost + "/i/0b9c4c55-1d1e-4a35-8e0c-3f1f0bb7a1aa:latest"
	if err := remote.Write(mustRef(t, ownRef), own, remote.WithAuth(&authn.Basic{Username: "observatory", Password: "build-secret-1"})); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	dockerfile := "ARG UBUNTU=" + pub + "/library/ubuntu:24.04\nFROM $UBUNTU AS base\nFROM " + ownRef + "\nFROM " + pub + "/missing:1\nFROM base\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Secret: "build-secret-1"}
	var out strings.Builder
	got := a.resolveBases(context.Background(), &out, dir, &url.URL{Scheme: "http", Host: hubHost})
	amdDigest, _ := amd.Digest()
	ownDigest, _ := own.Digest()
	want := []BaseDigest{{pub + "/library/ubuntu:24.04", amdDigest.String()}, {ownRef, ownDigest.String()}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !strings.Contains(out.String(), "base image "+pub+"/missing:1 not resolved") {
		t.Fatalf("log %q", out.String())
	}

	// Without the credential the hub registry refuses, and the base is left out.
	a.Secret = "wrong"
	if got := a.resolveBases(context.Background(), io.Discard, dir, &url.URL{Scheme: "http", Host: hubHost}); len(got) != 1 {
		t.Fatalf("wrong secret resolved %v", got)
	}
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
