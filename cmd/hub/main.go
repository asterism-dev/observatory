package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/api"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hub failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return errors.New("OBSERVATORY_PUBLIC_URL must be an absolute URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	oidc, err := auth.NewOIDC(ctx, cfg.Issuer, cfg.WebClientID, cfg.CLIClientID, strings.TrimRight(cfg.PublicURL, "/")+"/auth/callback")
	if err != nil {
		return err
	}
	s := store.New(pool).WithMasterKey(cfg.SecretsKey)
	if err := s.CheckMasterKey(ctx); err != nil {
		return err
	}
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		return err
	}
	nh, err := network.Load(ctx, s, z, network.Config{GatewayHost: cfg.GatewayHost, GatewayToken: cfg.GatewayToken})
	if err != nil {
		return err
	}
	filled, err := s.BackfillMgmtIPs(ctx)
	if err != nil {
		return err
	}
	slog.Info("management addresses backfilled", "vms", filled)
	a := &auth.Handler{Store: s, Auth: oidc, PublicURL: publicURL, Issuer: cfg.Issuer, CLIClientID: cfg.CLIClientID}

	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	a.Routes(mux)
	f := fleet.New(s, z, nh)
	api.NodeEndpoints(mux, s, f)
	api.GatewayEndpoints(mux, nh)
	nodeAssets(mux, cfg.NodeBinary)
	mux.Handle("/api/", api.New(s, a, z, f, nh))
	mux.Handle("/", spa(dist))

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("hub listening", "addr", cfg.Listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// spa serves index.html for extensionless paths that are not files, so GUI routes like /vms survive a reload.
func spa(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if _, err := fs.Stat(dist, strings.TrimPrefix(p, "/")); err != nil && path.Ext(p) == "" {
			http.ServeFileFS(w, r, dist, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}
