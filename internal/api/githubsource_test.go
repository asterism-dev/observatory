package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/store"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

type ghEntry struct {
	name, body, link string
	mode             int64
	typ              byte
}

func makeTarball(t *testing.T, entries ...ghEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: e.mode, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarballServer(t *testing.T, body []byte) gitHubSource {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return gitHubSource{API: srv.URL}
}

func TestGitHubResolve(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.WriteHeader(status)
		w.Write([]byte(testSHA))
	}))
	defer srv.Close()
	g := gitHubSource{API: srv.URL}
	ctx := context.Background()

	sha, err := g.resolve(ctx, "t", "o/r", "main")
	if err != nil || sha != testSHA || gotPath != "/repos/o/r/commits/main" || gotAuth != "Bearer t" || gotAccept != "application/vnd.github.sha" {
		t.Fatalf("got %q %v path=%q auth=%q accept=%q", sha, err, gotPath, gotAuth, gotAccept)
	}
	if _, err = g.resolve(ctx, "", "o/r", ""); err != nil || gotPath != "/repos/o/r/commits/HEAD" || gotAuth != "" {
		t.Fatalf("err %v path=%q auth=%q", err, gotPath, gotAuth)
	}
	for _, code := range []int{404, 422} {
		status = code
		if _, err = g.resolve(ctx, "", "o/r", "x"); !errors.Is(err, errSourceNotFound) {
			t.Fatalf("%d: %v", code, err)
		}
	}
	status = http.StatusServiceUnavailable
	if _, err = g.resolve(ctx, "", "o/r", "x"); !errors.Is(err, errSourceUnreachable) {
		t.Fatalf("503: %v", err)
	}
	status = http.StatusForbidden
	if _, err = g.resolve(ctx, "", "o/r", "x"); err == nil || errors.Is(err, errSourceNotFound) || errors.Is(err, errSourceUnreachable) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403: %v", err)
	}
	srv.Close()
	if _, err = g.resolve(ctx, "", "o/r", "x"); !errors.Is(err, errSourceUnreachable) {
		t.Fatalf("closed: %v", err)
	}
}

func fileSummary(files []store.RecipeFile) []string {
	var out []string
	for _, f := range files {
		out = append(out, fmt.Sprintf("%s:%o", f.Path, f.Mode))
	}
	slices.Sort(out)
	return out
}

func TestGitHubFiles(t *testing.T) {
	g := tarballServer(t, makeTarball(t,
		ghEntry{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		ghEntry{name: "o-r-abc123/", typ: tar.TypeDir, mode: 0o755},
		ghEntry{name: "o-r-abc123/img/Dockerfile", body: "FROM x", mode: 0o664},
		ghEntry{name: "o-r-abc123/img/bin/run", body: "#!/bin/sh", mode: 0o755},
		ghEntry{name: "o-r-abc123/img2/Dockerfile", body: "other", mode: 0o644},
		ghEntry{name: "o-r-abc123/README.md", body: "hi", mode: 0o644},
		ghEntry{name: "o-r-abc123/img/link", typ: tar.TypeSymlink, link: "Dockerfile"},
	))
	files, logs, err := g.files(context.Background(), "t", "o/r", "abc123", "img")
	if err != nil {
		t.Fatal(err)
	}
	if got := fileSummary(files); !slices.Equal(got, []string{"Dockerfile:644", "bin/run:755"}) {
		t.Fatalf("got %v", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "link") {
		t.Fatalf("logs %v", logs)
	}

	files, _, err = g.files(context.Background(), "", "o/r", "abc123", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := fileSummary(files); !slices.Equal(got, []string{"README.md:644", "img/Dockerfile:644", "img/bin/run:755", "img2/Dockerfile:644"}) {
		t.Fatalf("got %v", got)
	}
}

func TestGitHubFilesRejectsEscapes(t *testing.T) {
	g := tarballServer(t, makeTarball(t,
		ghEntry{name: "o-r-abc/../x", body: "x", mode: 0o644},
		ghEntry{name: "/abs", body: "x", mode: 0o644},
		ghEntry{name: "o-r-abc/ok", body: "x", mode: 0o644},
	))
	files, _, err := g.files(context.Background(), "", "o/r", "abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "ok" {
		t.Fatalf("got %+v", files)
	}
}

func TestGitHubFilesTooLarge(t *testing.T) {
	g := tarballServer(t, makeTarball(t, ghEntry{name: "o-r-abc/big", body: strings.Repeat("a", maxRecipeBytes+1), mode: 0o644}))
	_, _, err := g.files(context.Background(), "", "o/r", "abc", "")
	if err == nil || !strings.Contains(err.Error(), "50 MB") {
		t.Fatalf("got %v", err)
	}
}

func TestGitHubTarballRedirectDropsAuth(t *testing.T) {
	var cdnAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		w.Write(makeTarball(t, ghEntry{name: "o-r-abc/Dockerfile", body: "FROM x", mode: 0o644}))
	}))
	defer cdn.Close()
	var apiAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		// 127.0.0.1 and localhost are different hosts for the redirect check.
		http.Redirect(w, r, strings.Replace(cdn.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusFound)
	}))
	defer api.Close()
	files, _, err := gitHubSource{API: api.URL}.files(context.Background(), "secret", "o/r", "abc", "")
	if err != nil || len(files) != 1 {
		t.Fatalf("%v %v", files, err)
	}
	if apiAuth != "Bearer secret" || cdnAuth != "" {
		t.Fatalf("api auth %q, cdn auth %q", apiAuth, cdnAuth)
	}
}

func TestParseGitHubSource(t *testing.T) {
	ok := map[string]store.ImageSource{
		"o/r":              {Repo: "o/r"},
		"o/r@v1.2:img/sub": {Repo: "o/r", Ref: "v1.2", Path: "img/sub"},
		"o/r:img":          {Repo: "o/r", Path: "img"},
		"o/r@feature/x":    {Repo: "o/r", Ref: "feature/x"},
		"Owner-1/a.b_c-d":  {Repo: "Owner-1/a.b_c-d"},
	}
	for in, want := range ok {
		if got, err := parseGitHubSource(in); err != nil || got != want {
			t.Errorf("%q: got %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "o", "o/r/x", "o/..", "o/.", "o/r:../x", "o/r:/abs", "-o/r", "o/r@", "o/r:", "o//r", "o/r@a b"} {
		if got, err := parseGitHubSource(in); err == nil {
			t.Errorf("%q: accepted as %+v", in, got)
		}
	}
}
