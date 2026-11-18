package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigRoundTripPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "observatory")
	want := config{Hub: "http://hub.test", Token: "tok-test", ExpiresAt: testNow.Add(30 * 24 * time.Hour), Org: "o1"}
	if err := saveConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	checkModes(t, dir)
}

func checkModes(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
	}
}

func TestSaveConfigTightensMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "observatory")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(dir, config{Token: "tok-test"}); err != nil {
		t.Fatal(err)
	}
	checkModes(t, dir)
}

func TestConfigDirPrefersXDG(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": "/x"}
	got, err := configDir(func(k string) string { return env[k] })
	if err != nil || got != "/x/observatory" {
		t.Fatalf("got %q, %v", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err = configDir(func(string) string { return "" })
	if err != nil || got != filepath.Join(base, "observatory") {
		t.Fatalf("got %q, %v, want under %q", got, err, base)
	}
}

func TestLoadConfigMigratesCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "observatory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"hub":"http://h","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (config{Hub: "http://h", Token: "t"}) {
		t.Fatalf("got %+v", cfg)
	}
	if err := saveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("dir holds %v, want only config.json", entries)
	}
}
