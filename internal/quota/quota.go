// Package quota is the quota service: limits per org, team and member in schema quota, served at /api/quota/v1/.
package quota

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"

	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	"github.com/asterism-dev/observatory/internal/platform/auditlog"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
	"github.com/asterism-dev/observatory/internal/platform/rest"
	"github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
)

// prefix is where the edge routes the service; the handler strips it.
const prefix = "/api/quota/v1"

const maxBody = 1 << 20

const minTokenLen = 32

type Config struct {
	DatabaseURL string
	// IAMURL is iam's internal address for signing keys and the policy stream; Issuer is the public URL.
	IAMURL, Issuer, ServiceToken string
	NATSURL, NATSToken           string
	// ServiceTokens (name → token) authenticate other services' calls to /internal/quota/.
	ServiceTokens map[string]string
}

// Run serves the limits API on ln and relays quota's audit events until ctx ends.
func Run(ctx context.Context, cfg Config, ln net.Listener) error {
	defer ln.Close()
	if len(cfg.ServiceToken) < minTokenLen {
		return fmt.Errorf("quota: service token must be at least %d characters", minTokenLen)
	}
	v, err := identity.NewVerifier(cfg.IAMURL, cfg.Issuer, cfg.ServiceToken, nil)
	if err != nil {
		return err
	}
	w, err := authz.NewWatcher(cfg.IAMURL, cfg.ServiceToken, nil)
	if err != nil {
		return err
	}
	cat, err := Catalog()
	if err != nil {
		return fmt.Errorf("quota catalog: %w", err)
	}
	catalogs := iamv1connect.NewCatalogInternalServiceClient(&http.Client{Timeout: 30 * time.Second}, strings.TrimRight(cfg.IAMURL, "/")+"/internal/iam",
		connect.WithInterceptors(servicetoken.Bearer(cfg.ServiceToken)))
	digest, err := catalog.Register(ctx, catalogs, cat)
	if err != nil {
		return unlessStopped(ctx, err)
	}
	// Requests answer Unavailable until the policy stream carries this catalog.
	w.Require(cat.Service, digest)
	m, err := migrations()
	if err != nil {
		return err
	}
	pool, err := pgsvc.Open(ctx, cfg.DatabaseURL, "quota", m)
	if err != nil {
		return unlessStopped(ctx, err)
	}
	defer pool.Close()
	js, closeBus, err := bus.Await(ctx, cfg.NATSURL, cfg.NATSToken)
	if err != nil {
		return unlessStopped(ctx, err)
	}
	defer closeBus()
	if err := bus.EnsureStream(ctx, js); err != nil {
		return unlessStopped(ctx, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { w.Run(ctx) })
	wg.Go(func() { outbox.NewRelay(pool, js).Run(ctx) })

	// ponytail: no audit settings source yet, so reads are never recorded as data events; give quota an audit token when an org needs them.
	path, h := quotav1connect.NewQuotaServiceHandler(&server{store: &store{pool: pool}, policy: w},
		connect.WithInterceptors(auditlog.Interceptors("quota", v, w, nil, pool)...), connect.WithReadMaxBytes(maxBody))
	tc, err := vanguard.NewTranscoder([]*vanguard.Service{vanguard.NewService(path, h, vanguard.WithMaxMessageBufferBytes(maxBody))})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// Bodies are read before authz, so cap them; MaxBytesHandler also stops Connect from draining an oversized one.
	mux.Handle(prefix+"/", http.MaxBytesHandler(http.StripPrefix(prefix, rest.NoQuery(path, tc)), maxBody))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	err = run.Serve(ctx, srv, ln)
	cancel()
	wg.Wait()
	return err
}

// unlessStopped drops a start-up error caused by ctx ending, so a shutdown during start is no failure.
func unlessStopped(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}
