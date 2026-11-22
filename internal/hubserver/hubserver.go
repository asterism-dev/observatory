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

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/api"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/db"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	iamapi "github.com/asterism-dev/observatory/internal/iam"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/network"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
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
	oidc, err := auth.NewOIDC(ctx, cfg.Issuer, cfg.WebClientID, cfg.CLIClientID, strings.TrimRight(cfg.PublicURL, "/")+"/auth/callback")
	if err != nil {
		return err
	}
	s := store.New(pool).WithMasterKey(cfg.SecretsKey)
	if err := s.CheckMasterKey(ctx); err != nil {
		return err
	}
	// iam's own policy: no owner refs, since those are the hub's to add.
	iamZ, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		return err
	}
	selfToken := rand.Text() + rand.Text()
	cfg.ServiceTokens[selfService] = selfToken
	selfURL := "http://" + dialAddr(ln.Addr())
	// The hub verifies edge-issued JWTs against iam's JWKS, over HTTP, like any other service.
	verifier, err := identity.NewVerifier(iamURL(ln), cfg.PublicURL, selfToken, nil)
	if err != nil {
		return err
	}
	internalClient := &http.Client{Timeout: 30 * time.Second}
	withToken := connect.WithInterceptors(servicetoken.Bearer(selfToken))
	iam := iamv1connect.NewIAMInternalServiceClient(internalClient, iamURL(ln)+"/internal/iam", withToken)

	mux := http.NewServeMux()
	policies, err := mountIdentity(ctx, mux, s, iamZ, cfg)
	if err != nil {
		return err
	}
	// The hub serves iam's routes before its own policy exists, since it takes that policy from iam's stream; other requests wait.
	ready := make(chan struct{})
	var handler http.Handler
	srvCtx, stopSrv := context.WithCancel(ctx)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
			handler.ServeHTTP(w, r)
			return
		default:
		}
		if strings.HasPrefix(r.URL.Path, "/internal/iam/") {
			mux.ServeHTTP(w, r)
			return
		}
		select {
		case <-ready:
			handler.ServeHTTP(w, r)
		case <-srvCtx.Done():
			http.Error(w, "hub stopped before it was ready", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	served := make(chan struct{})
	var serveErr error
	go func() { serveErr = run.Serve(srvCtx, srv, ln); close(served) }()
	defer func() { stopSrv(); <-served }()

	watcher, err := platformauthz.NewWatcher(iamURL(ln), selfToken, nil)
	if err != nil {
		return err
	}
	hp, err := watchPolicy(ctx, watcher, s)
	if err != nil {
		return err
	}
	z := hp.Service
	nh, err := network.Load(ctx, s, z, network.Config{GatewayHost: cfg.GatewayHost, GatewayToken: cfg.GatewayToken})
	if err != nil {
		return err
	}
	filled, err := s.BackfillMgmtIPs(ctx)
	if err != nil {
		return err
	}
	slog.Info("management addresses backfilled", "vms", filled)
	reloadAfterLogin := func(ctx context.Context) error {
		if err := iamZ.Reload(ctx); err != nil {
			return err
		}
		// The SPA calls hub routes right after login, so wait until the hub has the new user's personal org.
		wctx, cancel := context.WithTimeout(ctx, loginPolicyWait)
		defer cancel()
		return hp.WaitRevision(wctx, policies.Revision())
	}
	a := &auth.Handler{Store: s, Auth: oidc, PublicURL: publicURL, Issuer: cfg.Issuer, CLIClientID: cfg.CLIClientID, Reload: reloadAfterLogin, EdgeToken: cfg.EdgeToken, Verifier: verifier, IAM: iam}
	a.Routes(mux)
	resources := api.ResourceServer{Store: s, Policy: hp}
	mux.Handle("/internal/hub/", servicetoken.Middleware(cfg.ServiceTokens)(internalHub(resources)))
	f, err := fleet.New(ctx, s, z, nh, iam)
	if err != nil {
		return err
	}
	defer f.Close()
	nh.OnChange(f.NotifyOrg)
	// iam can't reach the fleet, so the hub re-pushes the orgs whose IAM state a stream message changed.
	z.OnReload(func(old, new authz.Policy) {
		for _, org := range changedOrgs(old, new) {
			f.NotifyOrg(org)
		}
	})
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
		api.RegistryEndpoints(mux, s, z, api.Registry{Upstream: upstream, PublicURL: publicURL, OtherURLs: []*url.URL{cfg.NodeURL, builderHub}, Issuer: iss, Builds: s, Manifests: client, Tags: tags, EventsSecret: cfg.RegistryEventsSecret, GC: gc, IAM: iam})
		manifests = client
		f.PullImagesFrom(cfg.NodeURL, iss)
		f.BootBuildersWith(sched.BootEnv)
		api.BuildEndpoints(mux, s, sched, client, tags, buildLogs)
		go sched.Run(ctx, 10*time.Second)
		go api.NewAutoRebuilds(s, z, iam, sched, cfg.PublicURL, cfg.BuilderHubURL).Run(ctx, 10*time.Minute)
	}
	busCtx, stopBus := context.WithCancel(ctx)
	busDone := make(chan struct{})
	go func() {
		runBus(busCtx, pool, s, iam, f.NotifyOrg, z.LastKnown, manifests, cfg.NATSURL, cfg.NATSToken)
		close(busDone)
	}()
	// The bus must stop before the pool and fleet close under it, also when Run fails before ctx ends.
	defer func() { stopBus(); <-busDone }()
	go api.NewVMExpiry(s, z, f).Run(ctx, time.Minute)
	apiHandler := api.New(s, iam, a, z, f, nh, dh, ig, manifests, tags, sched, gc, cfg.DocsURL, newer, firecrackerVersion(string(installScript)), tc, cfg.LogsRetention)
	mux.Handle("/api/", apiHandler)
	hubResources := hubv1connect.NewResourceServiceClient(internalClient, selfURL+"/internal/hub", withToken)
	iamHandler, err := iamapi.Handler(iamapi.Deps{Store: s, Policy: iamZ, Verifier: verifier, Resources: hubResources, Pool: pool, PublicURL: publicURL, Revision: policies.Revision})
	if err != nil {
		return err
	}
	mux.Handle(iamapi.Prefix+"/", iamHandler)
	// The mux would redirect unclean paths; the Grafana proxy sees them raw and refuses them.
	handler = a.TrustEdge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/grafana/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	close(ready)
	slog.Info("hub listening", "addr", ln.Addr().String())
	<-served
	return serveErr
}

// iamURL is where the hub reaches iam: its own listener until iam runs apart; tests override it and firstPolicyWait, so hubserver tests must not run in parallel.
var iamURL = func(ln net.Listener) string { return "http://" + dialAddr(ln.Addr()) }

// loginPolicyWait bounds how long a login waits for its new user to reach the hub's policy.
const loginPolicyWait = 5 * time.Second

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

// runBus relays the outbox and consumes iam events once the bus is reachable; until then the hub serves and the outbox buffers.
func runBus(ctx context.Context, pool *pgxpool.Pool, s *store.Store, iam iamv1connect.IAMInternalServiceClient, notify func(uuid.UUID), policy func() *authz.Enforcer, manifests api.ManifestDeleter, url, token string) {
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
	consumed := make(chan struct{})
	go func() { consumeUntilDone(ctx, js, s, iam, notify, policy, manifests); close(consumed) }()
	defer func() { <-consumed }()
	outbox.NewRelay(pool, js).Run(ctx)
}

// consumeUntilDone restarts the iam event consumer whenever it fails; unpurged rows wait in the meantime.
func consumeUntilDone(ctx context.Context, js jetstream.JetStream, s *store.Store, iam iamv1connect.IAMInternalServiceClient, notify func(uuid.UUID), policy func() *authz.Enforcer, manifests api.ManifestDeleter) {
	for wait := time.Second; ; wait = min(2*wait, 30*time.Second) {
		err := consumeIAMEvents(ctx, js, s, iam, notify, policy, manifests)
		if ctx.Err() != nil {
			return
		}
		slog.Error("iam event consumer stopped, cleanup waits", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
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
	sched.PolicyFresh = func() bool { _, ok := z.Current(); return ok }
	sched.Size = cfg.BuilderSize
	return sched
}
