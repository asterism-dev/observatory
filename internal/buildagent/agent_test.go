package buildagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeHub struct {
	mu     sync.Mutex
	log    bytes.Buffer
	result map[string]string
	bad    int
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer build-secret-1" {
		h.bad++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/builds/b1/context":
		zw := gzip.NewWriter(w)
		tw := tar.NewWriter(zw)
		for name, body := range map[string]string{"Dockerfile": "FROM scratch\n", "src/a.txt": "a"} {
			_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			_, _ = tw.Write([]byte(body))
		}
		_ = tw.Close()
		_ = zw.Close()
	case "POST /api/builds/b1/log":
		_, _ = io.Copy(&h.log, r.Body)
	case "POST /api/builds/b1/result":
		_ = json.NewDecoder(r.Body).Decode(&h.result)
	default:
		http.NotFound(w, r)
	}
}

// fakeTools puts buildkitd, buildctl and vmm-token scripts on PATH; buildctl records its args and environment in dir.
func fakeTools(t *testing.T, dir, buildctlExit string) {
	t.Helper()
	bin := t.TempDir()
	scripts := map[string]string{
		"buildkitd": "exec sleep 60\n",
		"vmm-token": `[ "$1" = github ] && echo ghs_integration_token && exit 0; exit 3` + "\n",
		"buildctl": `[ "$1" = debug ] && exit 0
printf '%s\n' "$@" > ` + dir + `/args
cp "$DOCKER_CONFIG/config.json" ` + dir + `/docker.json
for a in "$@"; do case $a in id=*) src=${a#*,src=}; cp "$src" ` + dir + `/"${a%%,*}";; esac; done
while [ $# -gt 0 ]; do [ "$1" = --metadata-file ] && meta=$2; shift; done
echo "#1 building with npm token npm_s3cr3t_value" >&2
echo '{"containerimage.digest":"sha256:abc"}' > "$meta"
exit ` + buildctlExit + "\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func runAgent(t *testing.T, buildctlExit string) (*fakeHub, string, error) {
	t.Helper()
	hub := &fakeHub{}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	rec := t.TempDir()
	fakeTools(t, rec, buildctlExit)
	t.Setenv("OBSERVATORY_BUILD_URL", srv.URL)
	t.Setenv("OBSERVATORY_BUILD_ID", "b1")
	t.Setenv("OBSERVATORY_BUILD_SECRET", "build-secret-1")
	t.Setenv("OBSERVATORY_BUILD_IMAGE", "img-1")
	t.Setenv("OBSERVATORY_BUILD_SECRETS", "NPM_TOKEN")
	t.Setenv("OBSERVATORY_BUILD_INTEGRATIONS", "github")
	t.Setenv("NPM_TOKEN", "npm_s3cr3t_value")
	a, err := FromEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = a.Run(context.Background())
	return hub, rec, err
}

func TestAgentFlowWithFakes(t *testing.T) {
	hub, rec, err := runAgent(t, "0")
	if err != nil {
		t.Fatal(err)
	}
	if hub.bad != 0 {
		t.Errorf("%d requests without the build secret", hub.bad)
	}
	if hub.result["digest"] != "sha256:abc" || hub.result["error"] != "" {
		t.Errorf("result = %v", hub.result)
	}
	if log := hub.log.String(); !strings.Contains(log, "building with npm token ***") || strings.Contains(log, "npm_s3cr3t_value") {
		t.Errorf("log = %q, want the npm token masked", log)
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	host := strings.TrimPrefix(os.Getenv("OBSERVATORY_BUILD_URL"), "http://")
	for _, want := range []string{
		"--frontend\ndockerfile.v0\n",
		"--opt\nplatform=linux/amd64\n",
		"--secret\nid=github,src=",
		"--secret\nid=NPM_TOKEN,src=",
		"--output\ntype=image,name=" + host + "/i/img-1:build-b1,push=true\n",
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("buildctl args miss %q:\n%s", want, args)
		}
	}
	for file, want := range map[string]string{"id=github": "ghs_integration_token", "id=NPM_TOKEN": "npm_s3cr3t_value"} {
		if got, _ := os.ReadFile(filepath.Join(rec, file)); string(got) != want {
			t.Errorf("secret %s = %q, want %q", file, got, want)
		}
	}
	if strings.Contains(string(args), "attest:") {
		t.Errorf("buildctl args turn attestations on:\n%s", args)
	}
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	raw, _ := os.ReadFile(filepath.Join(rec, "docker.json"))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := base64.StdEncoding.DecodeString(cfg.Auths[host].Auth); string(got) != "observatory:build-secret-1" {
		t.Errorf("registry auth for %s = %q", host, got)
	}
}

func TestAgentReportsBuildError(t *testing.T) {
	hub, _, err := runAgent(t, "1")
	if err != nil {
		t.Fatal(err)
	}
	if hub.result["digest"] != "" || !strings.Contains(hub.result["error"], "buildctl") {
		t.Errorf("result = %v, want a buildctl error", hub.result)
	}
}
