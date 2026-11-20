package firecracker

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type apiRequest struct{ method, path, body string }

// fakeAPI serves status and reply to every request on a unix socket and records the requests.
func fakeAPI(t *testing.T, status int, reply string) (fcAPI, chan apiRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc") // t.TempDir can exceed the unix socket path limit on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	reqs := make(chan apiRequest, 10)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs <- apiRequest{r.Method, r.URL.Path, string(b)}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return apiAt(sock), reqs
}

func TestAPIPauseResume(t *testing.T) {
	api, reqs := fakeAPI(t, http.StatusNoContent, "")
	if err := api.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := api.Resume(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []apiRequest{{"PATCH", "/vm", `{"state":"Paused"}`}, {"PATCH", "/vm", `{"state":"Resumed"}`}} {
		if got := <-reqs; got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
}

func TestAPICreateSnapshot(t *testing.T) {
	api, reqs := fakeAPI(t, http.StatusNoContent, "")
	if err := api.CreateSnapshot("/vmstate", "/mem"); err != nil {
		t.Fatal(err)
	}
	want := apiRequest{"PUT", "/snapshot/create", `{"snapshot_type":"Full","snapshot_path":"/vmstate","mem_file_path":"/mem"}`}
	if got := <-reqs; got != want {
		t.Fatalf("got %+v", got)
	}
}

func TestAPILoadSnapshot(t *testing.T) {
	api, reqs := fakeAPI(t, http.StatusNoContent, "")
	if err := api.LoadSnapshot("/vmstate", "/mem", []netOverride{{IfaceID: "eth0", HostDevName: "t0"}}); err != nil {
		t.Fatal(err)
	}
	got := <-reqs
	if got.method != "PUT" || got.path != "/snapshot/load" {
		t.Fatalf("got %+v", got)
	}
	var body struct {
		Path    string `json:"snapshot_path"`
		Backend struct {
			Type string `json:"backend_type"`
			Path string `json:"backend_path"`
		} `json:"mem_backend"`
		Resume *bool         `json:"resume_vm"`
		Nets   []netOverride `json:"network_overrides"`
	}
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Path != "/vmstate" || body.Backend.Type != "File" || body.Backend.Path != "/mem" || body.Resume == nil || *body.Resume ||
		len(body.Nets) != 1 || body.Nets[0] != (netOverride{IfaceID: "eth0", HostDevName: "t0"}) {
		t.Fatalf("body = %s", got.body)
	}
}

func TestAPIPatchDrive(t *testing.T) {
	api, reqs := fakeAPI(t, http.StatusNoContent, "")
	if err := api.PatchDrive("d1", "/vdb"); err != nil {
		t.Fatal(err)
	}
	want := apiRequest{"PATCH", "/drives/d1", `{"drive_id":"d1","path_on_host":"/vdb"}`}
	if got := <-reqs; got != want {
		t.Fatalf("got %+v", got)
	}
}

// The restore path must configure the same logger a cold boot gets from fc.json.
func TestAPIPutLogger(t *testing.T) {
	api, reqs := fakeAPI(t, http.StatusNoContent, "")
	if err := api.PutLogger(); err != nil {
		t.Fatal(err)
	}
	cfg, err := vmConfig(testVMFile(), "k", "i", []string{"/root.img", "/seed.iso"})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Logger json.RawMessage `json:"logger"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, c.Logger); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.method != "PUT" || got.path != "/logger" || got.body != want.String() {
		t.Fatalf("got %+v, want body %s", got, want.String())
	}
}

func TestAPIFault(t *testing.T) {
	api, _ := fakeAPI(t, http.StatusBadRequest, `{"fault_message":"not paused"}`)
	err := api.CreateSnapshot("/vmstate", "/mem")
	if err == nil || !strings.Contains(err.Error(), "not paused") || !strings.Contains(err.Error(), "PUT /snapshot/create") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIFaultWithoutMessage(t *testing.T) {
	api, _ := fakeAPI(t, http.StatusInternalServerError, "boom")
	if err := api.Pause(); err == nil || !strings.HasSuffix(err.Error(), ": boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIBalloonStats(t *testing.T) {
	api, reqs := fakeAPI(t, http.StatusOK, `{"target_pages":0,"total_memory":4096,"available_memory":1024}`)
	st, err := api.BalloonStats()
	if err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; st != (balloonStats{TotalMemory: 4096, AvailableMemory: 1024}) || got.method != "GET" || got.path != "/balloon/statistics" {
		t.Fatalf("stats = %+v, request = %+v", st, got)
	}
}
