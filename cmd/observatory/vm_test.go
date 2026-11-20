package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	vmDev = `{"id":"v1","name":"dev","owner":{"kind":"user","id":"u1","name":"Ada Lovelace"},"node":{"id":"n1","name":"pve1"},"state":"running","power":"running","ip":"10.0.0.5","dnsName":"dev.internal","networks":[],"cpus":2,"memoryMB":4096,"diskGB":20,"image":"","legacyRuntime":true,"createdAt":"2026-11-19T13:00:00Z","disks":[],"autoSuspendMinutes":0}`
	vmWeb = `{"id":"v2","name":"web","owner":{"kind":"team","id":"t1","name":"backend"},"node":{"id":"n1","name":"pve1"},"state":"hibernated","power":"hibernated","dnsName":"web.internal","networks":[{"id":"w1","name":"lan","ip":"10.1.0.4","dnsName":"web.lan.internal"}],"cpus":4,"memoryMB":8192,"diskGB":30,"image":"v3","legacyRuntime":false,"createdAt":"2026-11-10T09:30:00Z","disks":[{"id":"d1","name":"data","mountPath":"/data","sizeGB":50,"state":"ready"}],"autoSuspendMinutes":30,"deleteAt":"2026-11-20T09:30:00Z"}`
)

func vmHub(t *testing.T, nodes string) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/vms", 200, `{"vms":[`+vmDev+`,`+vmWeb+`],"creatableOwners":[],"nodes":`+nodes+`}`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

const oneNode = `[{"id":"n1","name":"pve1","online":true}]`

func TestVMListGolden(t *testing.T) {
	_, c, out, _ := vmHub(t, oneNode)
	if code := run(c, []string{"vm", "list"}); code != 0 {
		t.Fatal(code)
	}
	golden(t, "vm-list.golden", out())
}

func TestVMShowGolden(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/v2", 200, vmWeb)
	if code := run(c, []string{"vm", "show", "web"}); code != 0 {
		t.Fatal(code)
	}
	golden(t, "vm-show.golden", out())
}

func TestVMCreateBody(t *testing.T) {
	h, c, _, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/teams", 200, `[{"id":"t1","name":"backend"}]`)
	h.on("POST /api/orgs/o1/vms", 201, vmDev)
	if code := run(c, []string{"vm", "create", "dev", "--team", "backend", "--cpus", "4", "--memory", "8G", "--disk", "30"}); code != 0 {
		t.Fatal(code)
	}
	want := `{"name":"dev","owner":{"kind":"team","teamId":"t1"},"nodeId":"n1","cpus":4,"memoryMB":8192,"diskGB":30}`
	if got := h.calls("POST /api/orgs/o1/vms")[0].Body; got != want {
		t.Errorf("body %s, want %s", got, want)
	}
}

func TestVMCreateImageFlag(t *testing.T) {
	h, c, _, _ := vmHub(t, oneNode)
	h.on("POST /api/orgs/o1/vms", 201, vmDev)
	if code := run(c, []string{"vm", "create", "dev", "--image", "org/base:v2"}); code != 0 {
		t.Fatal(code)
	}
	if b := h.calls("POST /api/orgs/o1/vms")[0].Body; !strings.Contains(b, `"image":"org/base:v2"`) {
		t.Errorf("body %s", b)
	}
}

func TestVMCreateSeveralNodes(t *testing.T) {
	h, c, _, errb := vmHub(t, `[{"id":"n1","name":"pve1"},{"id":"n2","name":"pve2"}]`)
	if code := run(c, []string{"vm", "create", "dev"}); code != 2 {
		t.Fatal(code)
	}
	if !strings.Contains(errb(), "several nodes; pass --node: pve1, pve2") || len(h.calls("POST /api/orgs/o1/vms")) != 0 {
		t.Errorf("stderr %q", errb())
	}
	h.on("POST /api/orgs/o1/vms", 201, vmDev)
	if code := run(c, []string{"vm", "create", "dev", "--node", "pve2", "--memory", "300M"}); code != 0 {
		t.Fatal(code)
	}
	if b := h.calls("POST /api/orgs/o1/vms")[0].Body; !strings.Contains(b, `"nodeId":"n2"`) || !strings.Contains(b, `"memoryMB":300`) || !strings.Contains(b, `"owner":{"kind":"user"}`) {
		t.Errorf("body %s", b)
	}
}

func TestVMCreateInvalidSize(t *testing.T) {
	h, c, _, errb := vmHub(t, oneNode)
	h.on("POST /api/orgs/o1/vms", 400, `{"error":"invalid size"}`)
	if code := run(c, []string{"vm", "create", "dev", "--memory", "300M"}); code != 1 || !strings.Contains(errb(), "invalid size") {
		t.Errorf("code, stderr %q", errb())
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int{"4G": 4096, "4GB": 4096, "512M": 512, "512": 512, "2g": 2048} {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	if _, err := parseMemory("lots"); err == nil {
		t.Error("want error")
	}
}

func stateSeq(h *fakeHub, path string, states ...string) {
	var n atomic.Int32
	h.onFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		i := min(int(n.Add(1))-1, len(states)-1)
		w.Write([]byte(states[i]))
	})
}

func vmState(state, errMsg string) string {
	s := strings.Replace(vmDev, `"state":"running"`, `"state":"`+state+`"`, 1)
	if errMsg != "" {
		s = strings.Replace(s, `"power"`, `"error":"`+errMsg+`","power"`, 1)
	}
	return s
}

func TestVMCreateWait(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("POST /api/orgs/o1/vms", 201, vmState("starting", ""))
	stateSeq(h, "/api/orgs/o1/vms/v1", vmState("starting", ""), vmState("running", ""))
	if code := run(c, []string{"vm", "create", "dev", "--wait"}); code != 0 || out() != "VM dev is running.\n" {
		t.Errorf("%d %q", code, out())
	}

	h2, c2, _, errb := vmHub(t, oneNode)
	h2.on("POST /api/orgs/o1/vms", 201, vmState("starting", ""))
	stateSeq(h2, "/api/orgs/o1/vms/v1", vmState("error", "no space"))
	if code := run(c2, []string{"vm", "create", "dev", "--wait"}); code != 1 || !strings.Contains(errb(), "no space") {
		t.Errorf("%d %q", code, errb())
	}
}

func TestVMPowerEndpoints(t *testing.T) {
	for _, tc := range []struct{ args, path string }{
		{"stop", "shutdown"}, {"stop --force", "stop"}, {"pause", "pause"},
		{"hibernate", "hibernate"}, {"resume", "resume"}, {"start", "start"},
	} {
		h, c, _, _ := vmHub(t, oneNode)
		h.on("POST /api/orgs/o1/vms/v1/"+tc.path, 200, vmDev)
		if code := run(c, append([]string{"vm"}, append(strings.Fields(tc.args), "dev")...)); code != 0 {
			t.Errorf("%s: %d", tc.args, code)
		}
		if len(h.calls("POST /api/orgs/o1/vms/v1/"+tc.path)) != 1 {
			t.Errorf("%s: no POST %s", tc.args, tc.path)
		}
	}
}

func TestVMStartWaitsForRunning(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("POST /api/orgs/o1/vms/v1/resume", 200, vmDev)
	stateSeq(h, "/api/orgs/o1/vms/v1", vmState("suspended", ""), vmState("running", ""))
	if code := run(c, []string{"vm", "resume", "dev", "--wait"}); code != 0 || out() != "VM dev is running.\n" {
		t.Errorf("%d %q", code, out())
	}
}

func TestVMRestartSequence(t *testing.T) {
	h, c, _, _ := vmHub(t, oneNode)
	h.on("POST /api/orgs/o1/vms/v1/shutdown", 200, vmDev)
	h.on("POST /api/orgs/o1/vms/v1/start", 200, vmDev)
	stateSeq(h, "/api/orgs/o1/vms/v1", vmState("running", ""), vmState("stopped", ""))
	if code := run(c, []string{"vm", "restart", "dev"}); code != 0 {
		t.Fatal(code)
	}
	var order []string
	for _, r := range h.reqs {
		if r.Path != "/api/me" && r.Path != "/api/orgs/o1/vms" {
			order = append(order, r.Method+" "+r.Path[strings.LastIndex(r.Path, "/")+1:])
		}
	}
	if got := strings.Join(order, ","); got != "POST shutdown,GET v1,GET v1,POST start" {
		t.Errorf("order %s", got)
	}

	h2, c2, _, _ := vmHub(t, oneNode)
	h2.on("GET /api/orgs/o1/vms", 200, `{"vms":[`+vmState("stopped", "")+`],"nodes":[]}`)
	h2.on("POST /api/orgs/o1/vms/v1/start", 200, vmDev)
	if code := run(c2, []string{"vm", "restart", "dev"}); code != 0 {
		t.Fatal(code)
	}
	for _, r := range h2.reqs {
		if r.Method == "POST" && !strings.HasSuffix(r.Path, "/start") {
			t.Errorf("unexpected %s", r.Path)
		}
	}
}

func TestVMDeleteConfirmsAndWaits(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("DELETE /api/orgs/o1/vms/v1", 204, "")
	if code := run(c, []string{"vm", "delete", "dev"}); code != 2 || len(h.calls("DELETE /api/orgs/o1/vms/v1")) != 0 {
		t.Fatalf("code %d", code)
	}
	stateSeq404 := 0
	h.onFunc("GET /api/orgs/o1/vms/v1", func(w http.ResponseWriter, r *http.Request) {
		stateSeq404++
		if stateSeq404 < 2 {
			w.Write([]byte(vmDev))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"not found"}`))
	})
	if code := run(c, []string{"vm", "delete", "dev", "--yes", "--wait"}); code != 0 || out() != "VM dev deleted.\n" {
		t.Errorf("%d %q", code, out())
	}
}

func TestVMNotFound(t *testing.T) {
	_, c, _, errb := vmHub(t, oneNode)
	if code := run(c, []string{"vm", "show", "nope"}); code != 5 || !strings.Contains(errb(), `vm "nope" not found`) {
		t.Errorf("%d %q", code, errb())
	}
}

func TestAutoSuspend(t *testing.T) {
	for arg, want := range map[string]string{"30": `{"minutes":30}`, "off": `{"minutes":0}`} {
		h, c, _, _ := vmHub(t, oneNode)
		h.on("PUT /api/orgs/o1/vms/v1/auto-suspend", 200, vmDev)
		if code := run(c, []string{"vm", "auto-suspend", "dev", arg}); code != 0 {
			t.Fatal(code)
		}
		if got := h.calls("PUT /api/orgs/o1/vms/v1/auto-suspend")[0].Body; got != want {
			t.Errorf("%s: %s", arg, got)
		}
	}
	h := newFakeHub(t)
	c, _, _ := newTestCLI(t, h)
	if code := run(c, []string{"vm", "auto-suspend", "dev", "3"}); code != 2 || len(h.reqs) != 0 {
		t.Errorf("code %d, %d requests", code, len(h.reqs))
	}
}

func TestVMJSON(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/v1", 200, `{"id":"v1","name":"dev"}`)
	if code := run(c, []string{"vm", "show", "dev", "--json"}); code != 0 || out() != "{\n  \"id\": \"v1\",\n  \"name\": \"dev\"\n}\n" {
		t.Errorf("%d %q", code, out())
	}
}

func TestVMMetricsEgressIntegrations(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/v1/metrics", 200, `{"samples":[{"at":"2026-11-19T14:00:00Z","cpuPercent":12.4,"memTotalMB":4096,"memUsedMB":1536}]}`)
	h.on("GET /api/orgs/o1/vms/v1/egress", 200, `{"secrets":2,"hosts":["api.example.com"],"caNotAfter":null,"trustChanged":false}`)
	h.on("GET /api/orgs/o1/vms/v1/integrations", 200, `{"integrations":[{"kind":"github","scope":"team","teamName":"backend","mode":"proxy"},{"kind":"claude"}]}`)
	for _, sub := range []string{"metrics", "egress", "integrations"} {
		if code := run(c, []string{"vm", sub, "dev"}); code != 0 {
			t.Fatalf("%s: %d", sub, code)
		}
	}
	for _, want := range []string{"12%", "1.5 GB / 4 GB", "api.example.com", "team backend", "claude"} {
		if !strings.Contains(out(), want) {
			t.Errorf("missing %q in\n%s", want, out())
		}
	}
}

func TestVMShowLegacyRuntimeHint(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/v1", 200, vmDev)
	if code := run(c, []string{"vm", "show", "dev"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out(), "old guest runtime, recreate this VM") {
		t.Fatalf("no recreate hint:\n%s", out())
	}
}

func TestVMShowVersionDigest(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/v2", 200, strings.Replace(vmWeb, `"image":"v3"`, `"image":"hub/i/x@sha256:child","imageRef":"org/tools","imageDigest":"sha256:index"`, 1))
	if code := run(c, []string{"vm", "show", "web"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out(), "sha256:index") || strings.Contains(out(), "sha256:child") {
		t.Errorf("stdout %s", out())
	}
}
