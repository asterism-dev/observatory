package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	_, err := loadConfig(func(string) string { return "" })
	if err == nil {
		t.Fatal("empty env accepted")
	}
	for _, k := range []string{"OBSERVATORY_DATABASE_URL", "OBSERVATORY_PUBLIC_URL", "OBSERVATORY_OIDC_ISSUER", "OBSERVATORY_OIDC_WEB_CLIENT_ID", "OBSERVATORY_OIDC_CLI_CLIENT_ID", "OBSERVATORY_SECRETS_KEY"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not name %s: %v", k, err)
		}
	}
	if strings.Contains(err.Error(), "LISTEN") {
		t.Errorf("listen reported as required: %v", err)
	}

	env := map[string]string{
		"OBSERVATORY_DATABASE_URL": "postgres://x", "OBSERVATORY_PUBLIC_URL": "http://h",
		"OBSERVATORY_OIDC_ISSUER": "http://i", "OBSERVATORY_OIDC_WEB_CLIENT_ID": "w", "OBSERVATORY_OIDC_CLI_CLIENT_ID": "c",
		"OBSERVATORY_SECRETS_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil || cfg.Listen != ":8090" || cfg.WebClientID != "w" {
		t.Fatalf("%+v, %v", cfg, err)
	}
	env["OBSERVATORY_LISTEN"] = ":1"
	if cfg, _ = loadConfig(func(k string) string { return env[k] }); cfg.Listen != ":1" {
		t.Fatalf("listen override: %+v", cfg)
	}

	env["OBSERVATORY_SECRETS_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err = loadConfig(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "OBSERVATORY_SECRETS_KEY") {
		t.Fatalf("short key: %v", err)
	}
}
