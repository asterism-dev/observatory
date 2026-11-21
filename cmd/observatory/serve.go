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
	"strings"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/edge"
	"github.com/asterism-dev/observatory/internal/hubserver"
	svcrun "github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/web"
)

func serveCommands() []command {
	return []command{{"serve", "edge|hub|all", "run observatory services", cmdServe}}
}

func cmdServe(c *cli, args []string) error {
	if len(args) != 1 || (args[0] != "edge" && args[0] != "hub" && args[0] != "all") {
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
		ln, err := listenEdge(c.getenv)
		if err != nil {
			return err
		}
		return serveEdge(ctx, edge.Config{HubURL: c.getenv("OBSERVATORY_EDGE_HUB_URL"), Token: c.getenv("OBSERVATORY_EDGE_TOKEN"), ServiceToken: c.getenv("OBSERVATORY_SERVICE_TOKEN")}, ln)
	default:
		ln, err := listenEdge(c.getenv)
		if err != nil {
			return err
		}
		return serveAll(ctx, c.getenv, ln)
	}
}

// serveAll runs the hub on a loopback port behind the edge on edgeLn, sharing edge and service tokens that live only in this process.
func serveAll(ctx context.Context, getenv func(string) string, edgeLn net.Listener) error {
	token := rand.Text() + rand.Text()
	serviceToken := rand.Text() + rand.Text()
	hubEnv := func(k string) string {
		switch k {
		case "OBSERVATORY_EDGE_TOKEN":
			return token
		case "OBSERVATORY_SERVICE_TOKENS":
			return "edge=" + serviceToken
		}
		return getenv(k)
	}
	hub := svcrun.Service{Name: "hub", Addr: hubListen(getenv, "all"), Run: func(ctx context.Context, ln net.Listener) error { return hubserver.Run(ctx, hubEnv, ln) }}
	return svcrun.All(ctx, edgeLn, func(services map[string]string, hubURL string) svcrun.Service {
		return svcrun.Service{Name: "edge", Run: func(ctx context.Context, ln net.Listener) error {
			return serveEdge(ctx, edge.Config{HubURL: hubURL, Token: token, ServiceToken: serviceToken, Services: services}, ln)
		}}
	}, []svcrun.Service{hub})
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
