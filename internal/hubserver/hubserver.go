package hubserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	// The hub image has no zoneinfo; TZ picks the zone OBSERVATORY_REGISTRY_GC_TIME is in.
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/api"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/db"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/iaminternal"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/release"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

// Run serves the hub on ln until ctx is done; the SPA and public routing are the edge's.
func Run(ctx context.Context, getenv func(string) string, ln net.Listener) error {
	defer ln.Close()
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return errors.New("OBSERVATORY_PUBLIC_URL must be an absolute URL")
	}

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan struct{})
	go func() { relayEvents(relayCtx, pool, cfg.NATSURL, cfg.NATSToken); close(relayDone) }()
	// The relay must stop before the pool closes under it, also when Run fails before ctx ends.
	defer func() { stopRelay(); <-relayDone }()

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
	// The hub verifies edge-issued JWTs against its own JWKS, over its own listener, like any other service.
	selfToken := rand.Text() + rand.Text()
	cfg.ServiceTokens[selfService] = selfToken
	verifier, err := identity.NewVerifier("http://"+dialAddr(ln.Addr()), cfg.PublicURL, selfToken, nil)
	if err != nil {
		return err
	}
	a := &auth.Handler{Store: s, Auth: oidc, PublicURL: publicURL, Issuer: cfg.Issuer, CLIClientID: cfg.CLIClientID, Reload: z.Reload, EdgeToken: cfg.EdgeToken, Verifier: verifier}

	mux := http.NewServeMux()
	a.Routes(mux)
	if err := mountIdentity(ctx, mux, s, z, cfg); err != nil {
		return err
	}
	// In-process until iam runs as its own service.
	iam := iaminternal.Server{Store: s}
	f, err := fleet.New(ctx, s, z, nh, iam)
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
	if release.Enabled(getenv) {
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
		sched = newScheduler(ctx, s, f, z, cfg)
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
		go api.NewAutoRebuilds(s, z, iam, sched, cfg.PublicURL, cfg.BuilderHubURL).Run(ctx, 10*time.Minute)
	}
	go api.NewVMExpiry(s, z, f).Run(ctx, time.Minute)
	apiHandler := api.New(s, iam, a, z, f, nh, dh, ig, manifests, tags, sched, gc, cfg.DocsURL, newer, firecrackerVersion(string(installScript)), tc, cfg.LogsRetention)
	mux.Handle("/api/", apiHandler)
	// The mux would redirect unclean paths; the Grafana proxy sees them raw and refuses them.
	handler := a.TrustEdge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/grafana/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	slog.Info("hub listening", "addr", ln.Addr().String())
	return run.Serve(ctx, srv, ln)
}

// dialAddr turns a listen address into one the hub can call itself on; a wildcard host becomes loopback.
func dialAddr(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	// Go's wildcard listener is dual-stack, and ::1 fails where IPv6 is disabled.
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// relayEvents publishes the outbox once the bus is reachable; until then the hub serves and the outbox buffers.
func relayEvents(ctx context.Context, pool *pgxpool.Pool, url, token string) {
	js, closeBus, err := bus.Await(ctx, url, token)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("event bus unusable, audit events wait in the outbox", "err", err)
		}
		return
	}
	defer closeBus()
	for wait := time.Second; ; wait = min(2*wait, 30*time.Second) {
		err := bus.EnsureStream(ctx, js)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("event stream unavailable, audit events wait in the outbox", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	outbox.NewRelay(pool, js).Run(ctx)
}

// newScheduler resolves the builder image once; if that fails, every build fails with the reason until the hub restarts.
func newScheduler(ctx context.Context, s *store.Store, f *fleet.Fleet, z *authz.Service, cfg config) *builds.Scheduler {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	builder, err := builds.ResolveBuilder(rctx, cfg.BuilderImage)
	if err != nil {
		slog.Error("builder image unavailable, image builds will fail", "err", err)
	} else {
		slog.Info("builder image", "ref", builder)
	}
	sched := builds.New(s, cfg.BuilderHubURL, builder, err, f.NotifyOrg, f.Online, func(org uuid.UUID, scope string) bool { return z.Enforcer().HasSubject(org, scope) })
	sched.Size = cfg.BuilderSize
	return sched
}
