package hubserver

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/seal"
)

type config struct {
	DatabaseURL, PublicURL, Issuer, WebClientID, CLIClientID, NodeBinary, Guestboot, GatewayToken, GatewayHost, StorageToken, EdgeToken string
	SecretsKey                                                                                                                          seal.Key
	Dev, DevSkipVerify                                                                                                                  bool
	DevClaudeTokenURL, DevClaudeUsageURL, DocsURL                                                                                       string
	RegistryURL, RegistryKeyDir, RegistryCertDir, RegistryEventsSecret                                                                  string
	// RegistryConfig is the registry's config file the hub runs garbage-collect with; RegistryGCTime is "HH:MM" or empty for manual runs.
	RegistryConfig, RegistryGCTime string
	// BuilderImage is the builder image repository; BuilderHubURL is the hub URL builder VMs reach, the public URL by default.
	BuilderImage, BuilderHubURL string
	BuilderSize                 builds.Size
	// NodeURL is the hub URL nodes pull images from, the public URL by default.
	NodeURL *url.URL
	// MetricsURL and LogsURL are the telemetry stores; both empty turns telemetry off.
	MetricsURL, LogsURL string
	// LogsRetention mirrors the log store's retention and bounds the logs explorer's time range.
	LogsRetention time.Duration
	// NATSURL and NATSToken reach the event bus the outbox relay publishes the hub's events to.
	NATSURL, NATSToken string
	// ServiceTokens maps service names to the tokens they call /internal/iam/ with; empty refuses every internal call.
	ServiceTokens map[string]string
}

const minTokenLen = 32

func loadConfig(getenv func(string) string) (config, error) {
	var cfg config
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
	required(&cfg.EdgeToken, "OBSERVATORY_EDGE_TOKEN")
	required(&cfg.NATSURL, "OBSERVATORY_NATS_URL")
	required(&cfg.NATSToken, "OBSERVATORY_NATS_TOKEN")
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
	if len(cfg.NATSToken) < minTokenLen {
		return config{}, fmt.Errorf("OBSERVATORY_NATS_TOKEN must be at least %d characters", minTokenLen)
	}
	if len(cfg.EdgeToken) < minTokenLen {
		return config{}, fmt.Errorf("OBSERVATORY_EDGE_TOKEN must be at least %d characters", minTokenLen)
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
	cfg.RegistryConfig = getenv("OBSERVATORY_REGISTRY_CONFIG")
	switch cfg.RegistryGCTime = getenv("OBSERVATORY_REGISTRY_GC_TIME"); cfg.RegistryGCTime {
	case "":
		cfg.RegistryGCTime = "04:00"
	case "off":
		cfg.RegistryGCTime = ""
	default:
		if err := registry.ParseSchedule(cfg.RegistryGCTime); err != nil {
			return config{}, fmt.Errorf("OBSERVATORY_REGISTRY_GC_TIME: %w", err)
		}
	}
	if cfg.BuilderImage = getenv("OBSERVATORY_BUILDER_IMAGE"); cfg.BuilderImage == "" {
		cfg.BuilderImage = builds.BuilderRepo
	}
	if cfg.BuilderHubURL = getenv("OBSERVATORY_BUILDER_HUB_URL"); cfg.BuilderHubURL == "" {
		cfg.BuilderHubURL = cfg.PublicURL
	}
	cfg.BuilderSize = builds.DefaultSize
	for _, f := range []struct {
		key string
		min int
		v   *int
	}{
		{"OBSERVATORY_BUILDER_CPUS", 1, &cfg.BuilderSize.CPUs},
		{"OBSERVATORY_BUILDER_MEMORY_MB", 1024, &cfg.BuilderSize.MemoryMB},
		{"OBSERVATORY_BUILDER_DISK_GB", 10, &cfg.BuilderSize.DiskGB},
	} {
		if s := getenv(f.key); s != "" {
			if *f.v, err = strconv.Atoi(s); err != nil || *f.v < f.min {
				return config{}, fmt.Errorf("%s must be a number of at least %d", f.key, f.min)
			}
		}
	}
	nodeURL := getenv("OBSERVATORY_NODE_URL")
	if nodeURL == "" {
		nodeURL = cfg.PublicURL
	}
	if cfg.NodeURL, err = url.Parse(nodeURL); err != nil || cfg.NodeURL.Host == "" || cfg.NodeURL.Scheme != "http" && cfg.NodeURL.Scheme != "https" {
		return config{}, errors.New("OBSERVATORY_NODE_URL must be an absolute http(s) URL")
	}
	cfg.MetricsURL, cfg.LogsURL = getenv("OBSERVATORY_METRICS_URL"), getenv("OBSERVATORY_LOGS_URL")
	if (cfg.MetricsURL == "") != (cfg.LogsURL == "") {
		return config{}, errors.New("OBSERVATORY_METRICS_URL and OBSERVATORY_LOGS_URL must be set together")
	}
	for key, v := range map[string]string{"OBSERVATORY_METRICS_URL": cfg.MetricsURL, "OBSERVATORY_LOGS_URL": cfg.LogsURL} {
		if u, err := url.Parse(v); v != "" && (err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https") {
			return config{}, fmt.Errorf("%s must be an absolute http(s) URL", key)
		}
	}
	if cfg.LogsRetention, err = parseRetention(getenv("OBSERVATORY_LOGS_RETENTION")); err != nil {
		return config{}, err
	}
	if cfg.ServiceTokens, err = servicetoken.Parse(getenv("OBSERVATORY_SERVICE_TOKENS")); err != nil {
		return config{}, fmt.Errorf("OBSERVATORY_SERVICE_TOKENS: %w", err)
	}
	if _, ok := cfg.ServiceTokens[selfService]; ok {
		return config{}, fmt.Errorf("OBSERVATORY_SERVICE_TOKENS: the name %q is reserved for the hub itself", selfService)
	}
	cfg.Dev = getenv("OBSERVATORY_DEV") == "1"
	skip := getenv("OBSERVATORY_DEV_SKIP_INTEGRATION_VERIFY")
	cfg.DevSkipVerify, cfg.DevClaudeTokenURL = skip == "1", getenv("OBSERVATORY_DEV_CLAUDE_TOKEN_URL")
	cfg.DevClaudeUsageURL = getenv("OBSERVATORY_DEV_CLAUDE_USAGE_URL")
	if !cfg.Dev && (skip != "" || cfg.DevClaudeTokenURL != "" || cfg.DevClaudeUsageURL != "") {
		return config{}, errors.New("OBSERVATORY_DEV_* requires OBSERVATORY_DEV=1")
	}
	return cfg, nil
}

// parseRetention reads the store's <n>h|d|w retention syntax, which time.ParseDuration cannot parse; empty is 14 days.
func parseRetention(v string) (time.Duration, error) {
	if v == "" {
		return 14 * 24 * time.Hour, nil
	}
	unit := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[v[len(v)-1]]
	n, err := strconv.Atoi(v[:len(v)-1])
	if unit == 0 || err != nil || n < 1 || n > math.MaxInt64/int(unit) || strings.ContainsAny(v, "+-") {
		return 0, errors.New("OBSERVATORY_LOGS_RETENTION must be <n>h, <n>d or <n>w")
	}
	return time.Duration(n) * unit, nil
}
