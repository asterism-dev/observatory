// Package audit is the audit service: it stores audit events from the bus in an append-only table and serves them.
package audit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	"github.com/asterism-dev/observatory/internal/platform/auditlog"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
	"github.com/asterism-dev/observatory/internal/platform/run"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
)

// prefix is where the edge routes the service; the handler strips it.
const prefix = "/api/audit/v1"

const maxBody = 1 << 20

const minTokenLen = 32

// internalPrefix serves other services; the edge never routes /internal/.
const internalPrefix = "/internal/audit"

type Config struct {
	DatabaseURL string
	// IAMURL is iam's internal address for signing keys and the policy stream; Issuer is the public URL.
	IAMURL, Issuer, ServiceToken string
	NATSURL, NATSToken           string
	// ServiceTokens (name → token) authenticate other services' calls to /internal/audit/.
	ServiceTokens map[string]string
}

// Run serves the read API on ln and consumes the bus until ctx ends.
func Run(ctx context.Context, cfg Config, ln net.Listener) error {
	defer ln.Close()
	if len(cfg.ServiceToken) < minTokenLen {
		return fmt.Errorf("audit: service token must be at least %d characters", minTokenLen)
	}
	v, err := identity.NewVerifier(cfg.IAMURL, cfg.Issuer, cfg.ServiceToken, nil)
	if err != nil {
		return err
	}
	w, err := authz.NewWatcher(cfg.IAMURL, cfg.ServiceToken, nil)
	if err != nil {
		return err
	}
	m, err := migrations()
	if err != nil {
		return err
	}
	pool, err := pgsvc.Open(ctx, cfg.DatabaseURL, "audit", m)
	if err != nil {
		return err
	}
	defer pool.Close()
	js, closeBus, err := bus.Await(ctx, cfg.NATSURL, cfg.NATSToken)
	if err != nil {
		return err
	}
	defer closeBus()
	if err := bus.EnsureStream(ctx, js); err != nil {
		return err
	}

	s := &store{pool: pool}
	internal := &internalServer{store: s}
	settings := auditlog.NewSettingsFrom(internal)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	consumeErr := make(chan error, 1)
	wg.Go(func() { w.Run(ctx) })
	wg.Go(func() { outbox.NewRelay(pool, js).Run(ctx) })
	wg.Go(func() { settings.Run(ctx, js.Conn()) })
	wg.Go(func() { runRetention(ctx, s) })
	wg.Go(func() {
		consumeErr <- consume(ctx, js, s)
		// A dead consumer stops the whole service rather than silently losing the trail.
		cancel()
	})

	// Bodies are read before authz, so cap them; MaxBytesHandler also stops Connect from draining an oversized one.
	path, h := auditv1connect.NewAuditServiceHandler(&server{store: s},
		connect.WithInterceptors(auditlog.Interceptors("audit", v, w, settings, pool)...), connect.WithReadMaxBytes(maxBody))
	mux := http.NewServeMux()
	mux.Handle(prefix+path, http.MaxBytesHandler(http.StripPrefix(prefix, h), maxBody))
	ipath, ih := auditv1connect.NewAuditInternalServiceHandler(internal, connect.WithReadMaxBytes(maxBody))
	mux.Handle(internalPrefix+ipath, http.MaxBytesHandler(http.StripPrefix(internalPrefix, servicetoken.Middleware(cfg.ServiceTokens)(ih)), maxBody))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	err = run.Serve(ctx, srv, ln)
	cancel()
	wg.Wait()
	return errors.Join(err, <-consumeErr)
}
