package auditlog

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
)

// ChangedSubject matches the audit service's audit.settings.changed events for every org and the platform.
const ChangedSubject = "events.*.audit.settings.changed"

const (
	minBackoff  = time.Second
	maxBackoff  = 30 * time.Second
	loadTimeout = 10 * time.Second
)

type orgService struct{ org, svc string }

// Settings holds which orgs record read calls per service; until the first load, none do.
type Settings struct {
	src auditv1connect.AuditInternalServiceClient
	cur atomic.Pointer[map[orgService]bool]
}

// NewSettings loads from the audit service's /internal/audit/ base URL with serviceToken.
func NewSettings(auditBaseURL, serviceToken string) (*Settings, error) {
	if auditBaseURL == "" || serviceToken == "" {
		return nil, errors.New("auditlog: audit url and service token are required")
	}
	bearer := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+serviceToken)
			return next(ctx, req)
		}
	})
	c := auditv1connect.NewAuditInternalServiceClient(&http.Client{Timeout: loadTimeout}, strings.TrimSuffix(auditBaseURL, "/"), connect.WithInterceptors(bearer))
	return NewSettingsFrom(c), nil
}

// NewSettingsFrom loads from src; the audit service passes its own handler.
func NewSettingsFrom(src auditv1connect.AuditInternalServiceClient) *Settings {
	return &Settings{src: src}
}

func (s *Settings) DataEvents(org, svc string) bool {
	m := s.cur.Load()
	return m != nil && (*m)[orgService{org, svc}]
}

// Run loads the settings now, on every audit.settings.changed event and after every NATS reconnect, until ctx ends.
func (s *Settings) Run(ctx context.Context, nc *nats.Conn) {
	reload := make(chan struct{}, 1)
	poke := func() {
		select {
		case reload <- struct{}{}:
		default:
		}
	}
	sub, err := nc.Subscribe(ChangedSubject, func(*nats.Msg) { poke() })
	if err != nil {
		slog.Error("audit settings: changes will not apply until restart", "err", err)
	} else {
		defer func() { _ = sub.Unsubscribe() }()
	}
	// Changes published while disconnected are lost to a core subscription, so a reconnect reloads; a status listener leaves the connection's handlers alone.
	connected := nc.StatusChanged(nats.CONNECTED)
	defer nc.RemoveStatusListener(connected)
	poke()
	backoff := minBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-connected:
		case <-reload:
		}
		if err := s.load(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("audit settings not loaded", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			poke()
			continue
		}
		backoff = minBackoff
	}
}

func (s *Settings) load(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	resp, err := s.src.GetDataEvents(ctx, connect.NewRequest(&auditv1.GetDataEventsRequest{}))
	if err != nil {
		return err
	}
	m := make(map[orgService]bool, len(resp.Msg.GetEnabled()))
	for _, e := range resp.Msg.GetEnabled() {
		m[orgService{e.GetOrg(), e.GetService()}] = true
	}
	s.cur.Store(&m)
	return nil
}
