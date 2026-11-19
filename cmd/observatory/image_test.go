package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	imgApp  = `{"id":"i-app","name":"app","description":"Web app","ref":"backend/app","scope":"team","ownerName":"backend","platform":false,"recipeRevision":3,"tags":{"latest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","v1":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"latestDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":157286400,"lastBuildState":"succeeded","pushError":"","createdAt":"2026-11-19T10:00:00Z"}`
	imgTool = `{"id":"i-tool","name":"tool","description":"","ref":"me/tool","scope":"user","ownerName":"Ada Lovelace","platform":false,"recipeRevision":null,"tags":{},"latestDigest":"","sizeBytes":0,"lastBuildState":"","pushError":"manifest is not linux/amd64","createdAt":"2026-11-19T13:00:00Z"}`
	imgStd  = `{"id":"i-std","name":"standard","description":"","ref":"observatory/standard","scope":"org","platform":true,"recipeRevision":null,"tags":{},"latestDigest":"","sizeBytes":0,"lastBuildState":"","pushError":"","createdAt":"2026-11-01T00:00:00Z"}`
)

func imageHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/images", 200, `[`+imgStd+`,`+imgApp+`,`+imgTool+`]`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func TestImageListGolden(t *testing.T) {
	_, c, out, errb := imageHub(t)
	mustRun(t, c, errb, 0, "image", "list")
	golden(t, "image-list.golden", out())
}

func TestImageShowGolden(t *testing.T) {
	h, c, out, errb := imageHub(t)
	h.on("GET /api/orgs/o1/images/i-tool", 200, imgTool)
	mustRun(t, c, errb, 0, "image", "show", "tool")
	golden(t, "image-show.golden", out())
}

func TestImageAmbiguousBareName(t *testing.T) {
	h, c, _, errb := imageHub(t)
	h.on("GET /api/orgs/o1/images", 200, `[`+imgApp+`,`+strings.NewReplacer("i-tool", "i-t2", `"tool"`, `"app"`, "me/tool", "me/app").Replace(imgTool)+`]`)
	mustRun(t, c, errb, 2, "image", "show", "app")
	if !strings.Contains(errb(), "backend/app, me/app") {
		t.Errorf("stderr %s", errb())
	}
}

func TestImageBuildsGolden(t *testing.T) {
	h, c, out, errb := imageHub(t)
	h.on("GET /api/orgs/o1/images/i-app/builds", 200, `[{"id":"b2","imageId":"i-app","revision":3,"state":"failed","queuedAt":"2026-11-19T14:00:00Z","digest":"","error":"step 4 failed"},{"id":"b1","imageId":"i-app","revision":2,"state":"succeeded","queuedAt":"2026-11-19T12:00:00Z","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","error":""}]`)
	mustRun(t, c, errb, 0, "image", "builds", "backend/app")
	golden(t, "image-builds.golden", out())
}

func TestImageVersionsGolden(t *testing.T) {
	h, c, out, errb := imageHub(t)
	h.on("GET /api/orgs/o1/images/i-app/versions", 200, `[{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source":"build","revision":3,"buildId":"b1","sizeBytes":157286400,"createdAt":"2026-11-19T12:30:00Z","tags":["latest"]},{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","source":"push","revision":null,"buildId":null,"sizeBytes":1024,"createdAt":"2026-11-18T09:00:00Z","tags":["v1"]}]`)
	mustRun(t, c, errb, 0, "image", "versions", "app")
	golden(t, "image-versions.golden", out())
}

func TestImageBuildFollowStreamsLog(t *testing.T) {
	h, c, out, errb := imageHub(t)
	h.on("POST /api/orgs/o1/images/i-app/builds", 202, `{"buildId":"b9"}`)
	h.onFunc("GET /api/orgs/o1/images/i-app/builds/b9/log", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") != "1" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		io.WriteString(w, "data: step 1\n\ndata: \x1b[31mred\n\nevent: end\ndata: failed\n\n")
	})
	h.on("GET /api/orgs/o1/images/i-app/builds/b9", 200, `{"id":"b9","state":"failed","error":"boom"}`)
	mustRun(t, c, errb, 1, "image", "build", "app", "--follow")
	if out() != "step 1\n?[31mred\n" || !strings.Contains(errb(), "build failed: boom") {
		t.Errorf("stdout %q stderr %q", out(), errb())
	}
}

func TestImageDefaultSet(t *testing.T) {
	h, c, _, errb := imageHub(t)
	h.on("GET /api/orgs/o1/teams", 200, `[{"id":"t1","name":"backend"}]`)
	h.on("PUT /api/orgs/o1/image-defaults/team/t1", 204, "")
	mustRun(t, c, errb, 0, "image", "default", "set", "team:backend", "backend/app:v1")
	if got := h.calls("PUT /api/orgs/o1/image-defaults/team/t1")[0].Body; got != `{"ref":"backend/app:v1"}` {
		t.Errorf("body %s", got)
	}
}

func TestImageVisibilityConfirmsVersionDeletion(t *testing.T) {
	h, c, _, errb := imageHub(t)
	n := 0
	h.onFunc("PATCH /api/orgs/o1/images/i-app", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			http.Error(w, `{"error":"widening visibility deletes 2 versions","code":"widen_deletes_versions"}`, http.StatusConflict)
			return
		}
		io.WriteString(w, imgApp)
	})
	mustRun(t, c, errb, 2, "image", "visibility", "app", "--scope", "org")
	n = 0
	mustRun(t, c, errb, 0, "image", "visibility", "app", "--scope", "org", "-y")
	calls := h.calls("PATCH /api/orgs/o1/images/i-app")
	if calls[len(calls)-1].Body != `{"scope":"org","confirmDeleteVersions":true}` {
		t.Errorf("body %s", calls[len(calls)-1].Body)
	}
}

func TestRecipePushTarsDir(t *testing.T) {
	h, c, _, errb := imageHub(t)
	h.on("GET /api/orgs/o1/teams", 200, `[{"id":"t1","name":"backend"}]`)
	var refsHeader, ctype string
	var names []string
	h.onFunc("PUT /api/orgs/o1/images/i-app/recipe", func(w http.ResponseWriter, r *http.Request) {
		refsHeader, ctype = r.Header.Get("X-Observatory-Refs"), r.Header.Get("Content-Type")
		io.WriteString(w, `{"revision":4}`)
	})
	dir := t.TempDir()
	for name, body := range map[string]string{
		"Dockerfile":          "FROM scratch\n",
		"app/main.go":         "package main\n",
		"node_modules/x/y.js": "x",
		"debug.log":           "x",
		"keep.log":            "x",
		"sub/trace.log":       "x",
		".dockerignore":       "# deps\nnode_modules\n*.log\n!keep.log\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, c, errb, 0, "image", "recipe", "push", "backend/app", dir, "--ref", "secret:org/NPM_TOKEN", "--ref", "integration:team:backend/github")
	zr, err := gzip.NewReader(strings.NewReader(h.calls("PUT /api/orgs/o1/images/i-app/recipe")[0].Body))
	if err != nil {
		t.Fatal(err)
	}
	for tr := tar.NewReader(zr); ; {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	slices.Sort(names)
	if want := []string{".dockerignore", "Dockerfile", "app/main.go", "keep.log", "sub/trace.log"}; !slices.Equal(names, want) {
		t.Errorf("tar holds %v, want %v", names, want)
	}
	if ctype != "application/gzip" {
		t.Errorf("content type %s", ctype)
	}
	if want := `[{"kind":"secret","scope":"org","name":"NPM_TOKEN"},{"kind":"integration","scope":"team","teamId":"t1","name":"github"}]`; refsHeader != want {
		t.Errorf("refs %s", refsHeader)
	}
}

func TestRecipePull(t *testing.T) {
	h, c, out, errb := imageHub(t)
	h.on("GET /api/orgs/o1/images/i-app/recipe", 200, `{"revision":3,"files":[{"path":"Dockerfile","mode":420,"size":5,"content":"FROM x"},{"path":"bin/blob","mode":493,"size":3,"content":null}],"refs":[{"kind":"secret","scope":"org","name":"NPM_TOKEN"}]}`)
	h.on("GET /api/orgs/o1/images/i-app/recipe/files/bin/blob", 200, "\x00\x01\x02")
	dir := filepath.Join(t.TempDir(), "out")
	mustRun(t, c, errb, 0, "image", "recipe", "pull", "app", dir)
	if b, _ := os.ReadFile(filepath.Join(dir, "bin", "blob")); string(b) != "\x00\x01\x02" {
		t.Errorf("blob %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile")); string(b) != "FROM x" {
		t.Errorf("Dockerfile %q", b)
	}
	if !strings.Contains(out(), "--ref secret:org/NPM_TOKEN") {
		t.Errorf("stdout %s", out())
	}
}

func TestImagePushShellsOutToDocker(t *testing.T) {
	h, c, _, errb := imageHub(t)
	_ = h
	bin, logFile := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	c.lookPath = exec.LookPath
	c.runProg = func(path string, argv []string) error {
		cmd := exec.Command(path, argv[1:]...)
		return cmd.Run()
	}
	mustRun(t, c, errb, 0, "image", "push", "backend/app:v2", "local:dev")
	host := strings.TrimPrefix(c.hub(), "http://")
	want := "tag local:dev " + host + "/i/i-app:v2\npush " + host + "/i/i-app:v2\n"
	if got, _ := os.ReadFile(logFile); string(got) != want {
		t.Errorf("docker calls %q, want %q", got, want)
	}
	mustRun(t, c, errb, 2, "image", "push", "observatory/standard", "local:dev")
}

func TestImageUpdateSendsOnlyGivenFields(t *testing.T) {
	h, c, _, errb := imageHub(t)
	h.on("PATCH /api/orgs/o1/images/i-app", 200, imgApp)
	mustRun(t, c, errb, 0, "image", "update", "app", "--description", "")
	mustRun(t, c, errb, 0, "image", "update", "app", "--name", "web")
	calls := h.calls("PATCH /api/orgs/o1/images/i-app")
	if calls[0].Body != `{"description":""}` || calls[1].Body != `{"name":"web"}` {
		t.Errorf("bodies %s %s", calls[0].Body, calls[1].Body)
	}
	mustRun(t, c, errb, 2, "image", "update", "app")
}

func TestImageCancel(t *testing.T) {
	h, c, out, errb := imageHub(t)
	h.on("GET /api/orgs/o1/images/i-app/builds", 200, `[{"id":"b9","imageId":"i-app","revision":3,"state":"running"}]`)
	h.on("GET /api/orgs/o1/images/i-tool/builds", 200, `[]`)
	h.on("POST /api/orgs/o1/images/i-app/builds/b9/cancel", 200, `{"id":"b9","state":"cancelled"}`)
	mustRun(t, c, errb, 0, "image", "cancel", "b9")
	if len(h.calls("POST /api/orgs/o1/images/i-app/builds/b9/cancel")) != 1 || !strings.Contains(out(), "Build b9 cancelled.") {
		t.Errorf("stdout %s", out())
	}
}

func TestImageTagAndVersionDelete(t *testing.T) {
	h, c, _, errb := imageHub(t)
	digest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h.on("DELETE /api/orgs/o1/images/i-app/tags/v1", 204, "")
	h.on("DELETE /api/orgs/o1/images/i-app/versions/"+digest, 204, "")
	mustRun(t, c, errb, 0, "image", "tag", "delete", "app", "v1")
	mustRun(t, c, errb, 2, "image", "versions", "delete", "app", digest)
	if n := len(h.calls("DELETE /api/orgs/o1/images/i-app/versions/" + digest)); n != 0 {
		t.Fatalf("deleted without confirmation: %d", n)
	}
	mustRun(t, c, errb, 0, "image", "versions", "delete", "app", digest, "-y")
	if len(h.calls("DELETE /api/orgs/o1/images/i-app/tags/v1")) != 1 || len(h.calls("DELETE /api/orgs/o1/images/i-app/versions/"+digest)) != 1 {
		t.Error("delete calls missing")
	}
}
