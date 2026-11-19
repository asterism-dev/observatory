package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testNow = time.Date(2026, 11, 19, 14, 3, 0, 0, time.UTC)

type recorded struct{ Method, Path, Query, Auth, Body string }

type fakeHub struct {
	*httptest.Server
	t      *testing.T
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	reqs   []recorded
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{t: t, routes: map[string]http.HandlerFunc{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		h.mu.Lock()
		h.reqs = append(h.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(body)})
		fn := h.routes[key]
		h.mu.Unlock()
		if fn == nil {
			t.Errorf("fake hub: unexpected request %s", key)
			http.Error(w, `{"error":"unexpected request"}`, http.StatusInternalServerError)
			return
		}
		fn(w, r)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHub) on(methodPath string, status int, body string) {
	h.onFunc(methodPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func (h *fakeHub) onFunc(methodPath string, fn http.HandlerFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[methodPath] = fn
}

func (h *fakeHub) calls(methodPath string) []recorded {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []recorded
	for _, r := range h.reqs {
		if r.Method+" "+r.Path == methodPath {
			out = append(out, r)
		}
	}
	return out
}

// newTestCLI returns a CLI with a temp HOME/XDG_CONFIG_HOME, logged in to h (if not nil) with tok-test.
func newTestCLI(t *testing.T, h *fakeHub) (c *cli, stdout, stderr *bytes.Buffer) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, k := range []string{"OBSERVATORY_HUB", "OBSERVATORY_ORG", "OBSERVATORY_TOKEN"} {
		t.Setenv(k, "")
	}
	stdout, stderr = new(bytes.Buffer), new(bytes.Buffer)
	c = &cli{
		ctx: t.Context(), stdin: strings.NewReader(""), stdout: stdout, stderr: stderr,
		getenv: os.Getenv, poll: time.Millisecond, http: &http.Client{Timeout: 10 * time.Second},
		exec: func(path string, argv, env []string) error {
			t.Errorf("unexpected exec %s %v", path, argv)
			return errors.New("exec not allowed in tests")
		},
		runProg: func(path string, argv []string) error {
			t.Errorf("unexpected run %s %v", path, argv)
			return errors.New("run not allowed in tests")
		},
		lookPath: func(name string) (string, error) { return "", errors.New(name + ": not found") },
		readPassword: func() ([]byte, error) {
			t.Errorf("unexpected readPassword")
			return nil, errors.New("no terminal")
		},
		now: func() time.Time { return testNow },
	}
	dir, err := configDir(c.getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{Token: "tok-test"}
	if h != nil {
		cfg.Hub = h.URL
	}
	if err := saveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	c.cfgDir, c.cfg = dir, cfg
	return c, stdout, stderr
}

const meOneOrgBody = `{"user":{"id":"u1","email":"ada@example.com","name":"Ada Lovelace","platformAdmin":false},"orgs":[{"id":"o1","name":"orga","personal":false,"role":"owner"}]}`

func meOneOrg(h *fakeHub) { h.on("GET /api/me", 200, meOneOrgBody) }

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}
