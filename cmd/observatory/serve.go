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
	"github.com/asterism-dev/observatory/internal/platform/bus"
	svcrun "github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/web"
)

func serveCommands() []command {
	return []command{{"serve", "edge|hub|audit|all", "run observatory services", cmdServe}}
}

func cmdServe(c *cli, args []string) error {
	if len(args) != 1 || (args[0] != "edge" && args[0] != "hub" && args[0] != "audit" && args[0] != "all") {
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
	case "audit":
		cfg := auditConfig(c.getenv)
		var missing []string
		for k, v := range map[string]string{"OBSERVATORY_AUDIT_DATABASE_URL or OBSERVATORY_DATABASE_URL": cfg.DatabaseURL, "OBSERVATORY_HUB_URL": cfg.HubURL,
			"OBSERVATORY_PUBLIC_URL": cfg.Issuer, "OBSERVATORY_SERVICE_TOKEN": cfg.ServiceToken, "OBSERVATORY_NATS_URL": cfg.NATSURL, "OBSERVATORY_NATS_TOKEN": cfg.NATSToken} {
			if v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
		}
		ln, err := net.Listen("tcp", envOr(c.getenv, "OBSERVATORY_AUDIT_LISTEN", ":8092"))
		if err != nil {
			return err
		}
		return audit.Run(ctx, cfg, ln)
	case "edge":
		var missing []string
		for _, k := range []string{"OBSERVATORY_EDGE_TOKEN", "OBSERVATORY_EDGE_HUB_URL", "OBSERVATORY_SERVICE_TOKEN"} {
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
		return serveEdge(ctx, edge.Config{HubURL: c.getenv("OBSERVATORY_EDGE_HUB_URL"), Token: c.getenv("OBSERVATORY_EDGE_TOKEN"), ServiceToken: c.getenv("OBSERVATORY_SERVICE_TOKEN"), Services: services}, ln)
	default:
		ln, err := listenEdge(c.getenv)
		if err != nil {
			return err
		}
		return serveAll(ctx, c.getenv, ln)
	}
}

// serveAll runs the hub and the audit service on loopback ports behind the edge on edgeLn, sharing edge and service tokens that live only in this process.
func serveAll(ctx context.Context, getenv func(string) string, edgeLn net.Listener) error {
	token := rand.Text() + rand.Text()
	serviceToken := rand.Text() + rand.Text()
	auditToken := rand.Text() + rand.Text()
	hubEnv := func(k string) string {
		switch k {
		case "OBSERVATORY_EDGE_TOKEN":
			return token
		case "OBSERVATORY_SERVICE_TOKENS":
			return "edge=" + serviceToken + ",audit=" + auditToken
		}
		return getenv(k)
	}
	auditCfg := auditConfig(getenv)
	auditCfg.ServiceToken = auditToken
	if auditCfg.NATSURL == "" {
		stop, err := embeddedNATS(ctx, getenv, &auditCfg)
		if err != nil {
			edgeLn.Close()
			return err
		}
		defer stop()
	}
	hub := svcrun.Service{Name: "hub", Addr: hubListen(getenv, "all"), Run: func(ctx context.Context, ln net.Listener) error { return hubserver.Run(ctx, hubEnv, ln) }}
	auditSvc := svcrun.Service{Name: "audit", Run: func(ctx context.Context, ln net.Listener) error { return audit.Run(ctx, auditCfg, ln) }}
	return svcrun.All(ctx, edgeLn, func(services map[string]string, hubURL string) svcrun.Service {
		// All calls this before starting any service, so the audit service sees the hub URL.
		auditCfg.HubURL = hubURL
		return svcrun.Service{Name: "edge", Run: func(ctx context.Context, ln net.Listener) error {
			return serveEdge(ctx, edge.Config{HubURL: hubURL, Token: token, ServiceToken: serviceToken, Services: services}, ln)
		}}
	}, []svcrun.Service{hub, auditSvc})
}

func auditConfig(getenv func(string) string) audit.Config {
	return audit.Config{
		DatabaseURL:  envOr(getenv, "OBSERVATORY_AUDIT_DATABASE_URL", getenv("OBSERVATORY_DATABASE_URL")),
		HubURL:       getenv("OBSERVATORY_HUB_URL"),
		Issuer:       getenv("OBSERVATORY_PUBLIC_URL"),
		ServiceToken: getenv("OBSERVATORY_SERVICE_TOKEN"),
		NATSURL:      getenv("OBSERVATORY_NATS_URL"),
		NATSToken:    getenv("OBSERVATORY_NATS_TOKEN"),
	}
}

// embeddedNATS starts an in-process bus for a single-process install, under OBSERVATORY_DATA_DIR or else a temporary directory removed on stop.
func embeddedNATS(ctx context.Context, getenv func(string) string, cfg *audit.Config) (func(), error) {
	dir, cleanup := filepath.Join(getenv("OBSERVATORY_DATA_DIR"), "nats"), func() {}
	if getenv("OBSERVATORY_DATA_DIR") == "" {
		tmp, err := os.MkdirTemp("", "observatory-nats-")
		if err != nil {
			return nil, err
		}
		dir, cleanup = tmp, func() { _ = os.RemoveAll(tmp) }
	}
	url, natsToken, stop, err := bus.Embedded(ctx, dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	cfg.NATSURL, cfg.NATSToken = url, natsToken
	return func() { stop(); cleanup() }, nil
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
