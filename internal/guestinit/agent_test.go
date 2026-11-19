package guestinit

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/version"
)

type steps struct {
	mu    sync.Mutex
	order []string
}

func (s *steps) rec(name string) {
	s.mu.Lock()
	s.order = append(s.order, name)
	s.mu.Unlock()
}

func testAgent(t *testing.T, seed proto.GuestSeed) (*Agent, *steps) {
	t.Helper()
	s := &steps{}
	a := &Agent{
		Root:          Root{t.TempDir()},
		Seed:          func() (proto.GuestSeed, error) { s.rec("seed"); return seed, nil },
		SetMACs:       func([]string) error { s.rec("macs"); return nil },
		SetClock:      func(int64) error { s.rec("clock"); return nil },
		ReseedRNG:     func([]byte) error { s.rec("rng"); return nil },
		SetHostname:   func(string) error { s.rec("hostname"); return nil },
		ConfigureNICs: func([]proto.GuestNIC) error { s.rec("nics"); return nil },
		GrowRoot:      func() error { s.rec("grow"); return nil },
		MountDisks:    func(Root, []proto.GuestDisk) error { s.rec("disks"); return nil },
		Sync:          func() { s.rec("sync") },
		StopDesktop:   func() { s.rec("desktop") },
	}
	a.SetReady()
	return a, s
}

var webSeed = proto.GuestSeed{Hostname: "web-1", InstanceID: "i-1", Env: map[string]string{"A": "1"}, SSHKeys: []string{"ssh-ed25519 AAAA one@host"}}

type reply struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Version string `json:"version"`
}

func call(t *testing.T, a *Agent, req string) reply {
	t.Helper()
	var r reply
	if err := json.Unmarshal(a.Handle([]byte(req)), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAgentResumeAppliesSeed(t *testing.T) {
	a, s := testAgent(t, webSeed)
	if r := call(t, a, `{"op":"resume","time":5}`); !r.OK {
		t.Fatalf("resume: %+v", r)
	}
	if got := get(t, a.Root, "etc/observatory/env.sh"); !strings.Contains(got, "export A='1'") {
		t.Fatalf("env.sh = %q", got)
	}
	if got := a.SeedKeys(); len(got) != 1 || got[0] != webSeed.SSHKeys[0] {
		t.Fatalf("seed keys = %v", got)
	}
	if a.Env()["A"] != "1" {
		t.Fatalf("env = %v", a.Env())
	}
	if strings.Join(s.order, ",") != "clock,seed" {
		t.Fatalf("steps = %v", s.order)
	}
}

func TestAgentTakeoffNewIdentity(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	old := "0123456789abcdef0123456789abcdef"
	put(t, a.Root, "etc/machine-id", old+"\n", 0o444)
	if r := call(t, a, `{"op":"takeoff","time":5,"entropy":"AAECAwQFBgcICQoLDA0ODw=="}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
	if id := strings.TrimSpace(get(t, a.Root, "etc/machine-id")); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) || id == old {
		t.Fatalf("machine-id = %q", id)
	}
	if h := get(t, a.Root, "etc/hostname"); h != "web-1\n" {
		t.Fatalf("hostname = %q", h)
	}
	if _, err := os.Stat(a.Root.join(hostKeyDir + "/ssh_host_ed25519_key")); err != nil {
		t.Fatal(err)
	}
}

func TestTakeoffOrder(t *testing.T) {
	a, s := testAgent(t, webSeed)
	if r := call(t, a, `{"op":"takeoff","time":5,"macs":["02:00:00:00:00:01"],"entropy":"AAECAwQFBgcICQoLDA0ODw=="}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
	want := "macs,clock,rng,seed,hostname,nics,grow,disks,sync,desktop"
	if got := strings.Join(s.order, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestTakeoffNeedsEntropy(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	if r := call(t, a, `{"op":"takeoff","time":5,"entropy":"AAEC"}`); r.OK || !strings.Contains(r.Error, "entropy") {
		t.Fatalf("reply = %+v", r)
	}
}

func TestTakeoffStepFailureIsReported(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	a.GrowRoot = func() error { return errors.New("resize2fs failed") }
	if r := call(t, a, `{"op":"takeoff","entropy":"AAECAwQFBgcICQoLDA0ODw=="}`); r.OK || r.Error != "resize2fs failed" {
		t.Fatalf("reply = %+v", r)
	}
}

func TestTakeoffToleratesBadCAs(t *testing.T) {
	seed := webSeed
	seed.EgressCAs = map[string]string{"x": "y"}
	a, _ := testAgent(t, seed)
	if r := call(t, a, `{"op":"takeoff","entropy":"AAECAwQFBgcICQoLDA0ODw=="}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
	if h := get(t, a.Root, "etc/hostname"); h != "web-1\n" {
		t.Fatalf("hostname = %q", h)
	}
}

func TestTakeoffHooksAreNilSafe(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	a.Sync, a.StopDesktop = nil, nil
	if r := call(t, a, `{"op":"takeoff","entropy":"AAECAwQFBgcICQoLDA0ODw=="}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
}

func TestPingReportsVersion(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	if r := call(t, a, `{"op":"ping"}`); !r.OK || r.Version != version.Version {
		t.Fatalf("reply = %+v", r)
	}
}

func TestPingWaitsForReady(t *testing.T) {
	a := &Agent{}
	got := make(chan reply, 1)
	go func() { got <- call(t, a, `{"op":"ping"}`) }()
	select {
	case r := <-got:
		t.Fatalf("ping answered before ready: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	a.SetReady()
	if r := <-got; !r.OK {
		t.Fatalf("reply = %+v", r)
	}
}

func TestOpsWaitForReady(t *testing.T) {
	a := &Agent{Root: Root{t.TempDir()}}
	put(t, a.Root, debBundle, "ROOTS\n", 0o644)
	b, _ := json.Marshal(map[string]any{"op": "trust", "certs": map[string]string{caID1: testPEM}})
	got := make(chan reply, 1)
	go func() { got <- call(t, a, string(b)) }()
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(get(t, a.Root, debBundle), testPEM) {
		t.Fatal("trust ran before ready")
	}
	a.SetReady()
	if r := <-got; !r.OK {
		t.Fatalf("reply = %+v", r)
	}
	if !strings.Contains(get(t, a.Root, debBundle), testPEM) {
		t.Fatal("trust did not run after ready")
	}
}

func TestUnknownFieldIgnored(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	if r := call(t, a, `{"op":"ping","extra":{"x":1}}`); !r.OK {
		t.Fatalf("reply = %+v", r)
	}
}

func TestAgentUnknownOp(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	if r := call(t, a, `{"op":"rm"}`); r.OK || r.Error != "unknown op" {
		t.Fatalf("reply = %+v", r)
	}
}

func TestAgentRejectsBadRequests(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	for _, tc := range []struct{ req, err string }{
		{`{"op":"takeoff","macs":["02:00:00:00:00:0G"]}`, "bad macs"},
		{`{"op":"takeoff","macs":["02:00:00:00:00:01:"]}`, "bad macs"},
		{`{"op":"takeoff","macs":["02:00:00:00:00:01","02:00:00:00:00:02","02:00:00:00:00:03","02:00:00:00:00:04","02:00:00:00:00:05","02:00:00:00:00:06","02:00:00:00:00:07","02:00:00:00:00:08","02:00:00:00:00:09","02:00:00:00:00:0a","02:00:00:00:00:0b","02:00:00:00:00:0c","02:00:00:00:00:0d","02:00:00:00:00:0e","02:00:00:00:00:0f","02:00:00:00:00:10","02:00:00:00:00:11"]}`, "bad macs"},
		{`{"op":"takeoff","entropy":"!!"}`, "bad entropy"},
		{`{"op":"takeoff","entropy":"` + strings.Repeat("A", 1028) + `"}`, "bad entropy"},
		{`{"op":"resume","time":true}`, "bad time"},
		{`{"op":"resume","time":-1}`, "bad time"},
		{`{"op":"resume","time":1.5}`, "bad time"},
		{`[]`, "bad request"},
		{`{"op":5}`, "bad request"},
		{`nope`, "bad request"},
	} {
		if r := call(t, a, tc.req); r.OK || r.Error != tc.err {
			t.Fatalf("%.60s: reply = %+v, want error %q", tc.req, r, tc.err)
		}
	}
}

func TestAgentTrustInstallsCerts(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	put(t, a.Root, debBundle, "ROOTS\n", 0o644)
	req := func(id, pem string) string {
		b, _ := json.Marshal(map[string]any{"op": "trust", "certs": map[string]string{id: pem}})
		return string(b)
	}
	if r := call(t, a, req(caID1, testPEM)); !r.OK {
		t.Fatalf("trust: %+v", r)
	}
	if got := get(t, a.Root, debBundle); !strings.Contains(got, testPEM) {
		t.Fatalf("bundle = %q", got)
	}
	want := get(t, a.Root, debBundle)
	for _, bad := range []string{req("../x", testPEM), req(caID1, testPEM+testKey), req(caID1, testKey), req(caID1, "x"), req(caID1, strings.Repeat(testPEM, 400))} {
		if r := call(t, a, bad); r.OK {
			t.Fatalf("%.80s: accepted", bad)
		}
	}
	if get(t, a.Root, debBundle) != want {
		t.Fatal("rejected request changed the bundle")
	}
}

func TestAgentTrustRejectsNonObjectCerts(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	for _, certs := range []string{`[]`, `""`, `0`, `false`, `null`} {
		if r := call(t, a, `{"op":"trust","certs":`+certs+`}`); r.OK || r.Error != "bad certs" {
			t.Fatalf("certs %s: reply = %+v", certs, r)
		}
	}
}

func TestServeAgent(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	client, server := net.Pipe()
	defer client.Close()
	served := false
	go a.ServeAgent(func() (agentConn, bool, error) {
		if served {
			select {}
		}
		served = true
		return server, true, nil
	})
	client.SetDeadline(time.Now().Add(5 * time.Second))
	client.Write([]byte(`{"op":"ping"}` + "\n"))
	var r reply
	if err := json.NewDecoder(client).Decode(&r); err != nil || !r.OK {
		t.Fatalf("reply = %+v, %v", r, err)
	}
}

func TestServeAgentRefusesGuestPeer(t *testing.T) {
	a, _ := testAgent(t, webSeed)
	client, server := net.Pipe()
	defer client.Close()
	served := false
	go a.ServeAgent(func() (agentConn, bool, error) {
		if served {
			select {}
		}
		served = true
		return server, false, nil
	})
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("read %d, %v: guest peer got an answer", n, err)
	}
}

func TestReadRequestLineTooLarge(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() { client.Write([]byte(strings.Repeat("x", 70<<10))) }()
	if _, err := readRequestLine(server); err == nil || err.Error() != "request too large" {
		t.Fatalf("err = %v", err)
	}
}
