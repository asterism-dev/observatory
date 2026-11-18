package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	devConfig = "[Interface]\nPrivateKey = KEY-dummy\nAddress = 10.9.0.2/32\n"
	devA      = `{"id":"d1","name":"laptop","ip":"10.9.0.2","owner":{"id":"u1","name":"Ada Lovelace"},"createdAt":"2026-11-10T09:00:00Z","lastHandshake":"2026-11-19T13:58:00Z","canDelete":true}`
	devB      = `{"id":"d2","name":"phone","ip":"10.9.0.3","owner":{"id":"u2","name":"Bob"},"createdAt":"2026-11-10T09:00:00Z","lastHandshake":null,"canDelete":false}`
)

func deviceHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("POST /api/orgs/o1/devices", 201, `{"device":`+devA+`,"config":"[Interface]\nPrivateKey = KEY-dummy\nAddress = 10.9.0.2/32\n"}`)
	h.on("GET /api/orgs/o1/devices", 200, `[`+devA+`,`+devB+`]`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func TestDeviceCreateWritesConfigOnce(t *testing.T) {
	_, c, out, errb := deviceHub(t)
	p := filepath.Join(t.TempDir(), "laptop.conf")
	if code := run(c, []string{"device", "create", "laptop", "--out", p}); code != 0 {
		t.Fatal(code, errb())
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, st)
	}
	if b, _ := os.ReadFile(p); string(b) != devConfig {
		t.Errorf("content %q", b)
	}
	if strings.Contains(out(), "PrivateKey") || strings.Contains(errb(), "PrivateKey") || !strings.Contains(out(), "Wrote "+p) {
		t.Errorf("stdout %q stderr %q", out(), errb())
	}
}

func TestDeviceCreateNeverOverwrites(t *testing.T) {
	h, c, _, errb := deviceHub(t)
	p := filepath.Join(t.TempDir(), "laptop.conf")
	os.WriteFile(p, []byte("old"), 0o600)
	if code := run(c, []string{"device", "create", "laptop", "--out", p}); code != 1 {
		t.Fatal(code)
	}
	if len(h.calls("POST /api/orgs/o1/devices")) != 0 || !strings.Contains(errb(), "exists; choose another --out") {
		t.Errorf("stderr %q", errb())
	}
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Errorf("file changed: %q", b)
	}
}

func TestDeviceCreateStdout(t *testing.T) {
	_, c, out, errb := deviceHub(t)
	if code := run(c, []string{"device", "create", "laptop", "--out", "-"}); code != 0 {
		t.Fatal(code, errb())
	}
	if out() != devConfig {
		t.Errorf("stdout %q", out())
	}
}

func TestDeviceCreateJSONOmitsConfig(t *testing.T) {
	_, c, out, errb := deviceHub(t)
	p := filepath.Join(t.TempDir(), "laptop.conf")
	if code := run(c, []string{"device", "create", "laptop", "--json", "--out", p}); code != 0 {
		t.Fatal(code, errb())
	}
	if !strings.Contains(out(), `"device"`) || strings.Contains(out(), "config") || strings.Contains(out(), "KEY-dummy") {
		t.Errorf("stdout %q", out())
	}
}

func TestDeviceListGolden(t *testing.T) {
	_, c, out, _ := deviceHub(t)
	if code := run(c, []string{"device", "list"}); code != 0 {
		t.Fatal(code)
	}
	golden(t, "device-list.golden", out())
}

func TestDeviceDeleteConfirms(t *testing.T) {
	h, c, _, errb := deviceHub(t)
	h.on("DELETE /api/orgs/o1/devices/d1", 204, "")
	if code := run(c, []string{"device", "delete", "laptop"}); code != 2 || !strings.Contains(errb(), "--yes") {
		t.Fatalf("code %d %q", code, errb())
	}
	if len(h.calls("DELETE /api/orgs/o1/devices/d1")) != 0 {
		t.Fatal("deleted without confirmation")
	}
	if code := run(c, []string{"device", "delete", "laptop", "--yes"}); code != 0 {
		t.Fatal(code)
	}
	if len(h.calls("DELETE /api/orgs/o1/devices/d1")) != 1 {
		t.Error("no delete")
	}
}
