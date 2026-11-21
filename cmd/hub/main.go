package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"
	// The hub image has no zoneinfo; TZ picks the zone OBSERVATORY_REGISTRY_GC_TIME is in.
	_ "time/tzdata"

	"github.com/asterism-dev/observatory/internal/api"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/db"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/release"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
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
	nh, err := network.Load(ctx, s, z, network.Config{GatewayHost: cfg.GatewayHost, GatewayToken: cfg.GatewayToken, GatewayNode: cfg.GatewayNode})
	if err != nil {
		return err
	}
	filled, err := s.BackfillMgmtIPs(ctx)
	if err != nil {
		return err
	}
	slog.Info("management addresses backfilled", "vms", filled)
	a := &auth.Handler{Store: s, Auth: oidc, PublicURL: publicURL, Issuer: cfg.Issuer, CLIClientID: cfg.CLIClientID, Reload: z.Reload}

	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	a.Routes(mux)
	f, err := fleet.New(ctx, s, z, nh)
	if err != nil {
		return err
	}
	defer f.Close()
	nh.OnChange(f.NotifyOrg)
	go f.SweepSnapshots(ctx, 10*time.Second, store.SnapshotCopyTimeout)
	go f.SweepEgressCAs(ctx, time.Hour)
	var tc *telemetry.Client
	if cfg.MetricsURL != "" {
		tc = &telemetry.Client{MetricsURL: cfg.MetricsURL, LogsURL: cfg.LogsURL}
	}
	var buildLogs *telemetry.LogWriter
	if tc != nil {
		buildLogs = telemetry.NewLogWriter(tc, telemetry.BuildLogStreamFields, 10000)
		logsDone := make(chan struct{})
		go func() { buildLogs.Run(ctx); close(logsDone) }()
		defer func() {
			if ctx.Err() == nil {
				return
			}
			select {
			case <-logsDone:
			case <-time.After(3 * time.Second):
			}
		}()
	}
	api.NodeEndpoints(mux, s, f, nh)
	api.OTLPEndpoints(mux, s, tc)
	api.GatewayEndpoints(mux, nh)
	dh := disks.New(s, cfg.StorageToken, f.NotifyOrg)
	api.StorageEndpoints(mux, dh)
	nodeAssets(mux, cfg.NodeBinary, cfg.Guestboot)
	oauth := &integrations.HTTPClaudeOAuth{TokenURL: cfg.DevClaudeTokenURL}
	google := integrations.HTTPGoogleOAuth{}
	go f.RefreshIntegrations(ctx, oauth, google, time.Minute)
	go f.PollUsage(ctx, integrations.HTTPClaudeUsage{URL: cfg.DevClaudeUsageURL}, time.Minute)
	ig := api.Integrations{Verifier: integrations.HTTPVerifier{}, OAuth: oauth, Google: google}
	if cfg.DevSkipVerify {
		ig.Verifier = integrations.SkipVerifier{}
		slog.Warn("dev mode: integration verification skipped")
	}
	if cfg.DevClaudeTokenURL != "" {
		slog.Warn("dev mode: claude token endpoint overridden", "url", cfg.DevClaudeTokenURL)
	}
	if cfg.DevClaudeUsageURL != "" {
		slog.Warn("dev mode: claude usage endpoint overridden", "url", cfg.DevClaudeUsageURL)
	}
	newer := func() string { return "" }
	if release.Enabled(os.Getenv) {
		ck := &release.Checker{URL: release.LatestURL, Client: &http.Client{Timeout: 10 * time.Second}}
		go ck.Run(ctx, 6*time.Hour)
		newer = ck.Newer
	}
	var manifests api.ManifestDeleter
	var tags *api.TagSync
	var sched *builds.Scheduler
	var gc *registry.GC
	if cfg.RegistryURL != "" {
		iss, err := registry.LoadIssuer(cfg.RegistryKeyDir)
		if err != nil {
			return err
		}
		if cfg.RegistryCertDir != "" {
			if err := iss.WriteCert(cfg.RegistryCertDir); err != nil {
				return err
			}
		}
		upstream, _ := url.Parse(cfg.RegistryURL)
		builderHub, err := url.Parse(cfg.BuilderHubURL)
		if err != nil {
			return fmt.Errorf("OBSERVATORY_BUILDER_HUB_URL: %w", err)
		}
		client := registry.Client{URL: cfg.RegistryURL, Issuer: iss}
		tags = api.NewTagSync(s, client)
		sched = newScheduler(ctx, s, f, cfg)
		if cfg.RegistryConfig != "" {
			gc = registry.NewGC([]string{"registry", "garbage-collect", cfg.RegistryConfig}, cfg.RegistryGCTime, func(ctx context.Context) (int, error) {
				active, err := s.ActiveBuilds(ctx)
				return len(slices.DeleteFunc(active, func(b store.ImageBuild) bool { return b.State != store.BuildRunning })), err
			}, func() { sched.Kick(); tags.Resume() })
			sched.MayStart = gc.BuildsMayStart
			tags.Held = func() bool { return !gc.PushAllowed() }
			go gc.Run(ctx)
		} else {
			slog.Warn("OBSERVATORY_REGISTRY_CONFIG is not set; the hub does not garbage-collect the registry")
		}
		api.RegistryEndpoints(mux, s, z, api.Registry{Upstream: upstream, PublicURL: publicURL, OtherURLs: []*url.URL{cfg.NodeURL, builderHub}, Issuer: iss, Builds: s, Manifests: client, Tags: tags, EventsSecret: cfg.RegistryEventsSecret, GC: gc})
		manifests = client
		f.PullImagesFrom(cfg.NodeURL, iss)
		f.BootBuildersWith(sched.BootEnv)
		api.BuildEndpoints(mux, s, sched, client, tags, buildLogs)
		go sched.Run(ctx, 10*time.Second)
		go api.NewAutoRebuilds(s, z, sched, cfg.PublicURL, cfg.BuilderHubURL).Run(ctx, 10*time.Minute)
	}
	go api.NewVMExpiry(s, z, f).Run(ctx, time.Minute)
	apiHandler := api.New(s, a, z, f, nh, dh, ig, manifests, tags, sched, gc, cfg.DocsURL, newer, firecrackerVersion(string(installScript)), tc, cfg.LogsRetention)
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", spa(dist))
	// The mux would redirect unclean paths; the Grafana proxy sees them raw and refuses them.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/grafana/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
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

// newScheduler resolves the builder image once; if that fails, every build fails with the reason until the hub restarts.
func newScheduler(ctx context.Context, s *store.Store, f *fleet.Fleet, cfg config) *builds.Scheduler {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	builder, err := builds.ResolveBuilder(rctx, cfg.BuilderImage)
	if err != nil {
		slog.Error("builder image unavailable, image builds will fail", "err", err)
	} else {
		slog.Info("builder image", "ref", builder)
	}
	sched := builds.New(s, cfg.BuilderHubURL, builder, err, f.NotifyOrg, f.Online)
	sched.Size = cfg.BuilderSize
	return sched
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
