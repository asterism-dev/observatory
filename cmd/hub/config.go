package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/seal"
)

type config struct {
	Listen, DatabaseURL, PublicURL, Issuer, WebClientID, CLIClientID, NodeBinary, Guestboot, GatewayToken, GatewayHost, StorageToken string
	SecretsKey                                                                                                                       seal.Key
	Dev, DevSkipVerify                                                                                                               bool
	DevClaudeTokenURL, DocsURL                                                                                                       string
	RegistryURL, RegistryKeyDir, RegistryCertDir, RegistryEventsSecret                                                               string
	// BuilderImage is the builder image repository; BuilderHubURL is the hub URL builder VMs reach, the public URL by default.
	BuilderImage, BuilderHubURL string
}

const minTokenLen = 32

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{Listen: getenv("OBSERVATORY_LISTEN")}
	if cfg.Listen == "" {
		cfg.Listen = ":8090"
	}
	if cfg.DocsURL = getenv("OBSERVATORY_DOCS_URL"); cfg.DocsURL == "" {
		cfg.DocsURL = "https://asterism-dev.github.io/observatory/"
	}
	cfg.NodeBinary = getenv("OBSERVATORY_NODE_BINARY")
	if cfg.NodeBinary == "" {
		cfg.NodeBinary = "bin/observatory-node-linux-amd64"
	}
	if cfg.Guestboot = getenv("OBSERVATORY_GUESTBOOT"); cfg.Guestboot == "" {
		cfg.Guestboot = "bin/guestboot.tar"
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
	required(&cfg.GatewayToken, "OBSERVATORY_GATEWAY_TOKEN")
	required(&cfg.StorageToken, "OBSERVATORY_STORAGE_TOKEN")
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
	if len(cfg.GatewayToken) < minTokenLen {
		return config{}, fmt.Errorf("OBSERVATORY_GATEWAY_TOKEN must be at least %d characters", minTokenLen)
	}
	if len(cfg.StorageToken) < minTokenLen {
		return config{}, fmt.Errorf("OBSERVATORY_STORAGE_TOKEN must be at least %d characters", minTokenLen)
	}
	if cfg.GatewayHost = getenv("OBSERVATORY_GATEWAY_HOST"); cfg.GatewayHost == "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || u.Hostname() == "" {
			return config{}, errors.New("OBSERVATORY_PUBLIC_URL must be an absolute URL")
		}
		cfg.GatewayHost = u.Hostname()
	}
	cfg.RegistryURL, cfg.RegistryKeyDir, cfg.RegistryCertDir = getenv("OBSERVATORY_REGISTRY_URL"), getenv("OBSERVATORY_REGISTRY_KEY_DIR"), getenv("OBSERVATORY_REGISTRY_CERT_DIR")
	if cfg.RegistryURL != "" {
		if u, err := url.Parse(cfg.RegistryURL); err != nil || u.Scheme == "" || u.Host == "" {
			return config{}, errors.New("OBSERVATORY_REGISTRY_URL must be an absolute URL")
		}
		if cfg.RegistryKeyDir == "" {
			return config{}, errors.New("OBSERVATORY_REGISTRY_URL requires OBSERVATORY_REGISTRY_KEY_DIR")
		}
		if cfg.RegistryEventsSecret = getenv("OBSERVATORY_REGISTRY_EVENTS_SECRET"); len(cfg.RegistryEventsSecret) < minTokenLen {
			return config{}, fmt.Errorf("OBSERVATORY_REGISTRY_URL requires OBSERVATORY_REGISTRY_EVENTS_SECRET of at least %d characters", minTokenLen)
		}
	}
	if cfg.BuilderImage = getenv("OBSERVATORY_BUILDER_IMAGE"); cfg.BuilderImage == "" {
		cfg.BuilderImage = builds.BuilderRepo
	}
	if cfg.BuilderHubURL = getenv("OBSERVATORY_BUILDER_HUB_URL"); cfg.BuilderHubURL == "" {
		cfg.BuilderHubURL = cfg.PublicURL
	}
	cfg.Dev = getenv("OBSERVATORY_DEV") == "1"
	skip := getenv("OBSERVATORY_DEV_SKIP_INTEGRATION_VERIFY")
	cfg.DevSkipVerify, cfg.DevClaudeTokenURL = skip == "1", getenv("OBSERVATORY_DEV_CLAUDE_TOKEN_URL")
	if !cfg.Dev && (skip != "" || cfg.DevClaudeTokenURL != "") {
		return config{}, errors.New("OBSERVATORY_DEV_* requires OBSERVATORY_DEV=1")
	}
	return cfg, nil
}
