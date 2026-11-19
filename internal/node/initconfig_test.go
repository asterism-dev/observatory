package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInitConfigCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "node.json")
	o := InitOptions{Path: path, HubURL: "http://h:8080", Credential: "secret-token", NodeID: "n1", MemoryMB: 1500}
	if err := InitConfig(o, "/var/lib/observatory-node/vms", "/var/lib/observatory-node/images"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	m := readJSON(t, path)
	hub := m["hub"].(map[string]any)
	paths := m["paths"].(map[string]any)
	if m["nodeId"] != "n1" || m["provider"] != "firecracker" || hub["url"] != "ws://h:8080/api/nodes/connect" || hub["credential"] != "secret-token" ||
		paths["vms"] != "/var/lib/observatory-node/vms" || paths["images"] != "/var/lib/observatory-node/images" ||
		m["capacity"].(map[string]any)["memoryMB"] != float64(1500) {
		t.Fatalf("config = %v", m)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
}

func TestInitConfigUpdatesOnlyGivenFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	old := `{"nodeId":"old","hub":{"url":"ws://old:1/api/nodes/connect","token":"old","extra":1},"provider":"vz",
"paths":{"vms":"/data/vms","images":"/data/img"},"capacity":{"cpus":3,"memoryMB":999},"vz":{"imageURL":"http://img"},"custom":[1,2]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	o := InitOptions{Path: path, HubURL: "https://hub.example:8443", Credential: "new", NodeID: "n1"}
	if err := InitConfig(o, "/x/vms", "/x/images"); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, path)
	hub := m["hub"].(map[string]any)
	if m["nodeId"] != "n1" || hub["url"] != "wss://hub.example:8443/api/nodes/connect" || hub["credential"] != "new" || hub["extra"] != float64(1) {
		t.Fatalf("hub = %v", m)
	}
	if p := m["paths"].(map[string]any); p["vms"] != "/data/vms" || p["images"] != "/data/img" {
		t.Fatalf("paths changed: %v", p)
	}
	if c := m["capacity"].(map[string]any); c["cpus"] != float64(3) || c["memoryMB"] != float64(999) {
		t.Fatalf("capacity changed: %v", c)
	}
	if m["vz"].(map[string]any)["imageURL"] != "http://img" || m["custom"] == nil {
		t.Fatalf("other keys lost: %v", m)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}

func TestInitConfigRejectsBadHub(t *testing.T) {
	for _, hub := range []string{"ftp://x:1", "http://x", "x:8080", ""} {
		o := InitOptions{Path: filepath.Join(t.TempDir(), "n.json"), HubURL: hub, Credential: "t", NodeID: "n"}
		if err := InitConfig(o, "a", "b"); err == nil {
			t.Errorf("%q accepted", hub)
		}
	}
}

func TestReadSecret(t *testing.T) {
	cred, err := ReadSecret(strings.NewReader("secret-token\n"))
	if err != nil || cred != "secret-token" {
		t.Fatalf("got %q, %v", cred, err)
	}
	if _, err := ReadSecret(strings.NewReader("")); err == nil {
		t.Fatal("empty stdin accepted")
	}
}

func TestInitConfigKeepsPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	first := InitOptions{Path: path, HubURL: "http://h:1", NodeID: "n1", Credential: "c1"}
	if err := InitConfig(first, "/a/vms", "/a/images"); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, path)
	m["firecracker"] = map[string]any{"kernel": "/k"}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	second := InitOptions{Path: path, HubURL: "https://h2:2", NodeID: "n2", Credential: "c2"}
	if err := InitConfig(second, "/b/vms", "/b/images"); err != nil {
		t.Fatal(err)
	}
	m = readJSON(t, path)
	hub := m["hub"].(map[string]any)
	if m["nodeId"] != "n2" || hub["url"] != "wss://h2:2/api/nodes/connect" || hub["credential"] != "c2" {
		t.Fatalf("hub not replaced: %v", m)
	}
	if m["paths"].(map[string]any)["vms"] != "/a/vms" || m["firecracker"].(map[string]any)["kernel"] != "/k" {
		t.Fatalf("sections lost: %v", m)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}

func TestCheckEnrollMemory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "node.json")
	if err := CheckEnrollMemory(missing, 2048); err != nil {
		t.Fatalf("explicit memory: %v", err)
	}
	if err := CheckEnrollMemory(writeConfig(t, fullConfig), 0); err != nil {
		t.Fatalf("existing config with capacity: %v", err)
	}
	_, want := defaultMemoryMB(totalMemoryMB())
	if err := CheckEnrollMemory(missing, 0); (err == nil) != (want == nil) {
		t.Fatalf("fresh config: err = %v, want %v", err, want)
	}
}
