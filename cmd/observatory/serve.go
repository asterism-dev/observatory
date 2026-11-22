package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/audit"
	"github.com/asterism-dev/observatory/internal/edge"
	"github.com/asterism-dev/observatory/internal/hubserver"
	"github.com/asterism-dev/observatory/internal/iam"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	svcrun "github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/quota"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/web"
)

func serveCommands() []command {
	return []command{{"serve", "edge|hub|iam|audit|quota|all", "run observatory services", cmdServe}}
}

func cmdServe(c *cli, args []string) error {
	if len(args) != 1 || !slices.Contains([]string{"edge", "hub", "iam", "audit", "quota", "all"}, args[0]) {
		return usagef("%s", usageLine("serve"))
	}
	ctx, stop := signal.NotifyContext(c.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restores the default handlers once shutdown starts, so a second signal during the drain exits at once.
	go func() { <-ctx.Done(); stop() }()
	if args[0] != "edge" && c.getenv("OBSERVATORY_LISTEN") != "" {
		return errors.New("OBSERVATORY_LISTEN is no longer read: set OBSERVATORY_HUB_LISTEN for the hub (OBSERVATORY_EDGE_LISTEN for the edge)")
	}
	switch args[0] {
	case "hub":
		ln, err := net.Listen("tcp", hubListen(c.getenv, "hub"))
		if err != nil {
			return err
		}
		return hubserver.Run(ctx, c.getenv, ln)
	case "iam":
		cfg, err := iamConfig(c.getenv)
		if err != nil {
			return err
		}
		secretsKey := ""
		if cfg.SecretsKey != (seal.Key{}) {
			secretsKey = "set"
		}
		if err := missingEnv(map[string]string{"OBSERVATORY_IAM_DATABASE_URL or OBSERVATORY_DATABASE_URL": cfg.DatabaseURL, "OBSERVATORY_PUBLIC_URL": cfg.PublicURL,
			"OBSERVATORY_OIDC_ISSUER": cfg.OIDCIssuer, "OBSERVATORY_OIDC_WEB_CLIENT_ID": cfg.WebClientID, "OBSERVATORY_OIDC_CLI_CLIENT_ID": cfg.CLIClientID,
			"OBSERVATORY_SECRETS_KEY": secretsKey, "OBSERVATORY_EDGE_TOKEN": cfg.EdgeToken, "OBSERVATORY_SERVICE_TOKEN": cfg.ServiceToken,
			"OBSERVATORY_HUB_URL": cfg.HubURL, "OBSERVATORY_AUDIT_URL": cfg.AuditURL, "OBSERVATORY_NATS_URL": cfg.NATSURL, "OBSERVATORY_NATS_TOKEN": cfg.NATSToken}); err != nil {
			return err
		}
		if cfg.ServiceTokens, err = servicetoken.Parse(c.getenv("OBSERVATORY_SERVICE_TOKENS")); err != nil {
			return fmt.Errorf("OBSERVATORY_SERVICE_TOKENS: %w", err)
		}
		ln, err := net.Listen("tcp", envOr(c.getenv, "OBSERVATORY_IAM_LISTEN", ":8093"))
		if err != nil {
			return err
		}
		return iam.Run(ctx, cfg, ln)
	case "audit":
		cfg := auditConfig(c.getenv)
		if err := missingEnv(map[string]string{"OBSERVATORY_AUDIT_DATABASE_URL or OBSERVATORY_DATABASE_URL": cfg.DatabaseURL, "OBSERVATORY_IAM_URL": cfg.IAMURL,
			"OBSERVATORY_PUBLIC_URL": cfg.Issuer, "OBSERVATORY_SERVICE_TOKEN": cfg.ServiceToken, "OBSERVATORY_NATS_URL": cfg.NATSURL, "OBSERVATORY_NATS_TOKEN": cfg.NATSToken}); err != nil {
			return err
		}
		var err error
		if cfg.ServiceTokens, err = servicetoken.Parse(c.getenv("OBSERVATORY_SERVICE_TOKENS")); err != nil {
			return fmt.Errorf("OBSERVATORY_SERVICE_TOKENS: %w", err)
		}
		ln, err := net.Listen("tcp", envOr(c.getenv, "OBSERVATORY_AUDIT_LISTEN", ":8092"))
		if err != nil {
			return err
		}
		return audit.Run(ctx, cfg, ln)
	case "quota":
		cfg := quotaConfig(c.getenv)
		if err := missingEnv(map[string]string{"OBSERVATORY_QUOTA_DATABASE_URL or OBSERVATORY_DATABASE_URL": cfg.DatabaseURL, "OBSERVATORY_IAM_URL": cfg.IAMURL,
			"OBSERVATORY_PUBLIC_URL": cfg.Issuer, "OBSERVATORY_SERVICE_TOKEN": cfg.ServiceToken, "OBSERVATORY_NATS_URL": cfg.NATSURL, "OBSERVATORY_NATS_TOKEN": cfg.NATSToken,
			"OBSERVATORY_AUDIT_URL": cfg.AuditURL}); err != nil {
			return err
		}
		var err error
		if cfg.ServiceTokens, err = servicetoken.Parse(c.getenv("OBSERVATORY_SERVICE_TOKENS")); err != nil {
			return fmt.Errorf("OBSERVATORY_SERVICE_TOKENS: %w", err)
		}
		if cfg.ServiceTokens["hub"] == "" {
			return errors.New("OBSERVATORY_SERVICE_TOKENS: hub= is required, the hub reserves quota")
		}
		ln, err := net.Listen("tcp", envOr(c.getenv, "OBSERVATORY_QUOTA_LISTEN", ":8094"))
		if err != nil {
			return err
		}
		return quota.Run(ctx, cfg, ln)
	case "edge":
		var missing []string
		for _, k := range []string{"OBSERVATORY_EDGE_TOKEN", "OBSERVATORY_EDGE_HUB_URL", "OBSERVATORY_EDGE_IAM_URL", "OBSERVATORY_SERVICE_TOKEN"} {
			if c.getenv(k) == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
		}
		services, err := edge.ParseServices(c.getenv("OBSERVATORY_EDGE_SERVICES"))
		if err != nil {
			return fmt.Errorf("OBSERVATORY_EDGE_SERVICES: %w", err)
		}
		ln, err := listenEdge(c.getenv)
		if err != nil {
			return err
		}
		return serveEdge(ctx, edge.Config{HubURL: c.getenv("OBSERVATORY_EDGE_HUB_URL"), IAMURL: c.getenv("OBSERVATORY_EDGE_IAM_URL"), Token: c.getenv("OBSERVATORY_EDGE_TOKEN"), ServiceToken: c.getenv("OBSERVATORY_SERVICE_TOKEN"), Services: services}, ln)
	default:
		ln, err := listenEdge(c.getenv)
		if err != nil {
			return err
		}
		return serveAll(ctx, c.getenv, ln)
	}
}

// serveAll runs iam, the hub, the audit and the quota service on loopback ports behind the edge on edgeLn, sharing edge and service tokens and the bus that live only in this process.
func serveAll(ctx context.Context, getenv func(string) string, edgeLn net.Listener) error {
	newToken := func() string { return rand.Text() + rand.Text() }
	edgeToken, edgeServiceToken, iamToken, hubToken, auditToken, quotaToken := newToken(), newToken(), newToken(), newToken(), newToken(), newToken()
	iamCfg, err := iamConfig(getenv)
	if err != nil {
		edgeLn.Close()
		return err
	}
	auditCfg := auditConfig(getenv)
	auditCfg.ServiceToken = auditToken
	auditCfg.ServiceTokens = map[string]string{"iam": iamToken, "quota": quotaToken}
	if auditCfg.NATSURL == "" {
		stop, err := embeddedNATS(ctx, getenv, &auditCfg)
		if err != nil {
			edgeLn.Close()
			return err
		}
		defer stop()
	}
	if err := missingEnv(map[string]string{"OBSERVATORY_OIDC_ISSUER": iamCfg.OIDCIssuer, "OBSERVATORY_OIDC_WEB_CLIENT_ID": iamCfg.WebClientID,
		"OBSERVATORY_OIDC_CLI_CLIENT_ID": iamCfg.CLIClientID}); err != nil {
		edgeLn.Close()
		return err
	}
	iamCfg.EdgeToken, iamCfg.ServiceToken, iamCfg.NATSURL, iamCfg.NATSToken = edgeToken, iamToken, auditCfg.NATSURL, auditCfg.NATSToken
	iamCfg.ServiceTokens = map[string]string{"edge": edgeServiceToken, "hub": hubToken, "audit": auditToken, "quota": quotaToken}
	quotaCfg := quotaConfig(getenv)
	quotaCfg.ServiceToken, quotaCfg.NATSURL, quotaCfg.NATSToken = quotaToken, auditCfg.NATSURL, auditCfg.NATSToken
	quotaCfg.ServiceTokens = map[string]string{"hub": hubToken}
	var iamURL string
	hubEnv := func(k string) string {
		switch k {
		case "OBSERVATORY_EDGE_TOKEN":
			return edgeToken
		case "OBSERVATORY_IAM_URL":
			return iamURL
		case "OBSERVATORY_SERVICE_TOKEN":
			return hubToken
		case "OBSERVATORY_SERVICE_TOKENS":
			return "iam=" + iamToken
		case "OBSERVATORY_NATS_URL":
			return auditCfg.NATSURL
		case "OBSERVATORY_NATS_TOKEN":
			return auditCfg.NATSToken
		}
		return getenv(k)
	}
	hub := svcrun.Service{Name: "hub", Addr: hubListen(getenv, "all"), Run: func(ctx context.Context, ln net.Listener) error { return hubserver.Run(ctx, hubEnv, ln) }}
	iamSvc := svcrun.Service{Name: "iam", Run: func(ctx context.Context, ln net.Listener) error { return iam.Run(ctx, iamCfg, ln) }}
	auditSvc := svcrun.Service{Name: "audit", Run: func(ctx context.Context, ln net.Listener) error { return audit.Run(ctx, auditCfg, ln) }}
	quotaSvc := svcrun.Service{Name: "quota", Run: func(ctx context.Context, ln net.Listener) error { return quota.Run(ctx, quotaCfg, ln) }}
	return svcrun.All(ctx, edgeLn, func(services map[string]string, hubURL string) svcrun.Service {
		// All calls this before starting any service, so every service sees the others' URLs.
		iamURL = services["iam"]
		auditCfg.IAMURL, quotaCfg.IAMURL = iamURL, iamURL
		iamCfg.HubURL, iamCfg.AuditURL, quotaCfg.AuditURL = hubURL, services["audit"], services["audit"]
		return svcrun.Service{Name: "edge", Run: func(ctx context.Context, ln net.Listener) error {
			return serveEdge(ctx, edge.Config{HubURL: hubURL, IAMURL: iamURL, Token: edgeToken, ServiceToken: edgeServiceToken, Services: services}, ln)
		}}
	}, []svcrun.Service{hub, iamSvc, auditSvc, quotaSvc})
}

// iamConfig reads iam's environment; serve iam checks it is complete, serve all fills in what it generates.
func iamConfig(getenv func(string) string) (iam.Config, error) {
	cfg := iam.Config{
		DatabaseURL:  envOr(getenv, "OBSERVATORY_IAM_DATABASE_URL", getenv("OBSERVATORY_DATABASE_URL")),
		PublicURL:    getenv("OBSERVATORY_PUBLIC_URL"),
		OIDCIssuer:   getenv("OBSERVATORY_OIDC_ISSUER"),
		WebClientID:  getenv("OBSERVATORY_OIDC_WEB_CLIENT_ID"),
		CLIClientID:  getenv("OBSERVATORY_OIDC_CLI_CLIENT_ID"),
		EdgeToken:    getenv("OBSERVATORY_EDGE_TOKEN"),
		HubURL:       getenv("OBSERVATORY_HUB_URL"),
		ServiceToken: getenv("OBSERVATORY_SERVICE_TOKEN"),
		AuditURL:     getenv("OBSERVATORY_AUDIT_URL"),
		NATSURL:      getenv("OBSERVATORY_NATS_URL"),
		NATSToken:    getenv("OBSERVATORY_NATS_TOKEN"),
	}
	if key := getenv("OBSERVATORY_SECRETS_KEY"); key != "" {
		var err error
		if cfg.SecretsKey, err = seal.ParseKey(key); err != nil {
			return iam.Config{}, fmt.Errorf("OBSERVATORY_SECRETS_KEY: %w", err)
		}
	}
	return cfg, nil
}

func missingEnv(values map[string]string) error {
	var missing []string
	for k, v := range values {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
}

func auditConfig(getenv func(string) string) audit.Config {
	return audit.Config{
		DatabaseURL:  envOr(getenv, "OBSERVATORY_AUDIT_DATABASE_URL", getenv("OBSERVATORY_DATABASE_URL")),
		IAMURL:       getenv("OBSERVATORY_IAM_URL"),
		Issuer:       getenv("OBSERVATORY_PUBLIC_URL"),
		ServiceToken: getenv("OBSERVATORY_SERVICE_TOKEN"),
		NATSURL:      getenv("OBSERVATORY_NATS_URL"),
		NATSToken:    getenv("OBSERVATORY_NATS_TOKEN"),
	}
}

func quotaConfig(getenv func(string) string) quota.Config {
	return quota.Config{
		DatabaseURL:  envOr(getenv, "OBSERVATORY_QUOTA_DATABASE_URL", getenv("OBSERVATORY_DATABASE_URL")),
		IAMURL:       getenv("OBSERVATORY_IAM_URL"),
		Issuer:       getenv("OBSERVATORY_PUBLIC_URL"),
		ServiceToken: getenv("OBSERVATORY_SERVICE_TOKEN"),
		NATSURL:      getenv("OBSERVATORY_NATS_URL"),
		NATSToken:    getenv("OBSERVATORY_NATS_TOKEN"),
		AuditURL:     getenv("OBSERVATORY_AUDIT_URL"),
	}
}

// embeddedNATS starts an in-process bus for a single-process install under OBSERVATORY_DATA_DIR/nats; without a persistent directory, unconsumed events would be lost on restart.
func embeddedNATS(ctx context.Context, getenv func(string) string, cfg *audit.Config) (func(), error) {
	dataDir := getenv("OBSERVATORY_DATA_DIR")
	if dataDir == "" {
		return nil, errors.New("set OBSERVATORY_DATA_DIR for the embedded event bus, or OBSERVATORY_NATS_URL and OBSERVATORY_NATS_TOKEN for an external one")
	}
	url, natsToken, stop, err := bus.Embedded(ctx, filepath.Join(dataDir, "nats"))
	if err != nil {
		return nil, err
	}
	cfg.NATSURL, cfg.NATSToken = url, natsToken
	return stop, nil
}

func hubListen(getenv func(string) string, svc string) string {
	if svc == "all" {
		// Without its own edge in front, a single-process install must not expose the hub API.
		return envOr(getenv, "OBSERVATORY_HUB_LISTEN", "127.0.0.1:8091")
	}
	return envOr(getenv, "OBSERVATORY_HUB_LISTEN", ":8091")
}

func listenEdge(getenv func(string) string) (net.Listener, error) {
	return net.Listen("tcp", envOr(getenv, "OBSERVATORY_EDGE_LISTEN", ":8090"))
}

func serveEdge(ctx context.Context, cfg edge.Config, ln net.Listener) error {
	defer ln.Close()
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return err
	}
	h, err := edge.New(cfg, dist)
	if err != nil {
		return err
	}
	// No WriteTimeout: SSE streams and WebSocket upgrades pass through the edge.
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	return svcrun.Serve(ctx, srv, ln)
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}
