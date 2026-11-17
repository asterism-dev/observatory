package main

import (
	"fmt"
	"strings"

	"github.com/asterism-dev/observatory/internal/seal"
)

type config struct {
	Listen, DatabaseURL, PublicURL, Issuer, WebClientID, CLIClientID, NodeBinary string
	SecretsKey                                                                   seal.Key
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{Listen: getenv("OBSERVATORY_LISTEN")}
	if cfg.Listen == "" {
		cfg.Listen = ":8090"
	}
	cfg.NodeBinary = getenv("OBSERVATORY_NODE_BINARY")
	if cfg.NodeBinary == "" {
		cfg.NodeBinary = "bin/observatory-node-linux-amd64"
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
	secretsKey := getenv("OBSERVATORY_SECRETS_KEY")
	if secretsKey == "" {
		missing = append(missing, "OBSERVATORY_SECRETS_KEY")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
	}
	var err error
	if cfg.SecretsKey, err = seal.ParseKey(secretsKey); err != nil {
		return config{}, fmt.Errorf("OBSERVATORY_SECRETS_KEY: %w", err)
	}
	return cfg, nil
}
