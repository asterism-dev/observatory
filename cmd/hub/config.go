package main

import (
	"fmt"
	"strings"
)

type config struct {
	Listen, DatabaseURL, PublicURL, Issuer, WebClientID, CLIClientID string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{Listen: getenv("OBSERVATORY_LISTEN")}
	if cfg.Listen == "" {
		cfg.Listen = ":8090"
	}
	var missing []string
	required := func(dst *string, key string) {
		if *dst = getenv(key); *dst == "" {
			missing = append(missing, key)
		}
	}
	required(&cfg.DatabaseURL, "OBSERVATORY_DATABASE_URL")
	required(&cfg.PublicURL, "OBSERVATORY_PUBLIC_URL")
	required(&cfg.Issuer, "OBSERVATORY_OIDC_ISSUER")
	required(&cfg.WebClientID, "OBSERVATORY_OIDC_WEB_CLIENT_ID")
	required(&cfg.CLIClientID, "OBSERVATORY_OIDC_CLI_CLIENT_ID")
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
