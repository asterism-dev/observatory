package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readConfigFile(t *testing.T, c *cli) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.cfgDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOrgUseStoresID(t *testing.T) {
	h := newFakeHub(t)
	h.on("GET /api/me", 200, meTwoOrgsBody)
	c, stdout, _ := newTestCLI(t, h)
	if code := run(c, []string{"org", "use", "orga"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := readConfigFile(t, c)["org"]; got != "o1" {
		t.Fatalf("config org %v, want o1", got)
	}
	if !strings.Contains(stdout.String(), "orga") {
		t.Errorf("stdout %q", stdout.String())
	}
	c, _, _ = newTestCLI(t, h)
	if code := run(c, []string{"org", "use", "nope"}); code != 5 {
		t.Errorf("unknown org: exit %d", code)
	}
}

func TestWhoami(t *testing.T) {
	h := newFakeHub(t)
	h.on("GET /api/me", 200, meTwoOrgsBody)
	c, stdout, _ := newTestCLI(t, h)
	if err := saveConfig(c.cfgDir, config{Hub: h.URL, Token: "tok-test", ExpiresAt: testNow.Add(30 * 24 * time.Hour), Org: "o2"}); err != nil {
		t.Fatal(err)
	}
	if code := run(c, []string{"whoami"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	golden(t, "whoami.golden", stdout.String())

	h401 := newFakeHub(t)
	h401.on("GET /api/me", 401, `{"error":"unauthorized"}`)
	c, _, stderr := newTestCLI(t, h401)
	if code := run(c, []string{"whoami"}); code != 3 {
		t.Fatalf("401: exit %d", code)
	}
	if !strings.Contains(stderr.String(), "session expired or revoked, run observatory login") {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestLogoutRevokesAndClears(t *testing.T) {
	h := newFakeHub(t)
	h.on("POST /auth/logout", 204, "")
	c, _, _ := newTestCLI(t, h)
	if err := saveConfig(c.cfgDir, config{Hub: h.URL, Token: "tok-test", ExpiresAt: testNow.Add(time.Hour), Org: "o1"}); err != nil {
		t.Fatal(err)
	}
	if code := run(c, []string{"logout"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := h.calls("POST /auth/logout"); len(got) != 1 || got[0].Auth != "Bearer tok-test" {
		t.Fatalf("logout calls %+v", got)
	}
	m := readConfigFile(t, c)
	if m["hub"] != h.URL || m["org"] != "o1" || m["token"] != nil || m["expiresAt"] != nil {
		t.Fatalf("config after logout %v", m)
	}

	h401 := newFakeHub(t)
	h401.on("POST /auth/logout", 401, `{"error":"unauthorized"}`)
	c, _, _ = newTestCLI(t, h401)
	if code := run(c, []string{"logout"}); code != 0 {
		t.Fatalf("logout with revoked session: exit %d", code)
	}
	if readConfigFile(t, c)["token"] != nil {
		t.Fatal("token kept")
	}
}

func TestLoginIgnoresCorruptConfig(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	if err := os.WriteFile(filepath.Join(c.cfgDir, "config.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	if code := run(c, []string{"login", "--hub", srv.URL}); code == 0 || !strings.Contains(stderr.String(), "fetch hub config") {
		t.Fatalf("login should get past the corrupt config: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	c.g = globals{}
	if code := run(c, []string{"whoami"}); code != 1 || strings.Contains(stderr.String(), "not logged in") {
		t.Fatalf("whoami should report the corrupt file: exit %d, stderr %q", code, stderr.String())
	}
}

func TestJSONPassThrough(t *testing.T) {
	h := newFakeHub(t)
	meOneOrg(h)
	body := `{"id":"o1","name":"orga","personal":false,"role":"owner","permissions":["vm.create","vm.read"]}`
	h.on("GET /api/orgs/o1", 200, body)
	c, stdout, _ := newTestCLI(t, h)
	if code := run(c, []string{"org", "show", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "{\n  \"id\": \"o1\",\n  \"name\": \"orga\",\n  \"personal\": false,\n  \"role\": \"owner\",\n  \"permissions\": [\n    \"vm.create\",\n    \"vm.read\"\n  ]\n}\n"
	if stdout.String() != want {
		t.Fatalf("got %q, want %q", stdout.String(), want)
	}
}
