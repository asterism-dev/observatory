package hubserver

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/store"
)

const policySendTimeout = 30 * time.Second

type streamControllerKey struct{}

// PolicyServer streams the authorization snapshot; Publish never blocks, and a slow watcher skips to the latest snapshot.
type PolicyServer struct {
	mu   sync.Mutex
	rev  uint64
	last *iamv1.Policy
	subs map[chan struct{}]struct{}
}

func NewPolicyServer() *PolicyServer {
	// Seeding with the clock keeps revisions increasing across hub restarts, so watchers never see one go backwards.
	return &PolicyServer{rev: uint64(time.Now().UnixMicro()), subs: map[chan struct{}]struct{}{}}
}

// Publish is fed from authz.Service.OnReload.
func (p *PolicyServer) Publish(pol authz.Policy) {
	msg := policyMessage(pol)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last != nil {
		msg.Revision = p.last.Revision
		if proto.Equal(msg, p.last) {
			return
		}
	}
	p.rev++
	msg.Revision = p.rev
	p.last = msg
	for ch := range p.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *PolicyServer) latest() *iamv1.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *PolicyServer) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch
}

func (p *PolicyServer) unsubscribe(ch chan struct{}) {
	p.mu.Lock()
	delete(p.subs, ch)
	p.mu.Unlock()
}

func (p *PolicyServer) WatchPolicy(ctx context.Context, _ *connect.Request[iamv1.WatchPolicyRequest], stream *connect.ServerStream[iamv1.Policy]) error {
	caller, _ := servicetoken.Caller(ctx)
	slog.Info("policy watch started", "caller", caller)
	defer slog.Info("policy watch ended", "caller", caller)
	ch := p.subscribe()
	defer p.unsubscribe(ch)
	var sent uint64
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		}
		msg := p.latest()
		if msg == nil || msg.Revision == sent {
			continue
		}
		if rc, ok := ctx.Value(streamControllerKey{}).(*http.ResponseController); ok {
			// A watcher that stops reading must not pin this goroutine forever.
			_ = rc.SetWriteDeadline(time.Now().Add(policySendTimeout))
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
		sent = msg.Revision
	}
}

func policyMessage(p authz.Policy) *iamv1.Policy {
	m := &iamv1.Policy{}
	for _, r := range p.Memberships {
		m.Memberships = append(m.Memberships, &iamv1.MemberRole{OrgId: r.OrgID.String(), UserId: r.UserID.String(), Role: r.Role})
	}
	for _, t := range p.TeamMembers {
		m.TeamMembers = append(m.TeamMembers, &iamv1.TeamMember{OrgId: t.OrgID.String(), TeamId: t.TeamID.String(), UserId: t.UserID.String(), Admin: t.Admin})
	}
	for _, r := range p.Roles {
		role := &iamv1.CustomRole{Id: r.ID.String(), OrgId: r.OrgID.String(), Name: r.Name, Description: r.Description, Parents: r.Parents}
		for _, rule := range r.Rules {
			role.Rules = append(role.Rules, &iamv1.Rule{Effect: string(rule.Effect), Object: rule.Object, Action: rule.Action})
		}
		m.Roles = append(m.Roles, role)
	}
	for _, b := range p.Bindings {
		m.Bindings = append(m.Bindings, &iamv1.Binding{OrgId: b.OrgID.String(), Subject: b.Subject, RoleId: b.RoleID.String()})
	}
	for _, t := range p.Teams {
		m.Teams = append(m.Teams, &iamv1.TeamRef{OrgId: t.OrgID.String(), Id: t.ID.String()})
	}
	return m
}

// selfService names the token the hub's own verifier fetches the JWKS with.
const selfService = "hub"

// mountIdentity serves the transitional identity issuer under /internal/iam/, fed by every successful z.Reload.
func mountIdentity(ctx context.Context, mux *http.ServeMux, s *store.Store, z *authz.Service, cfg config) error {
	jwtIssuer, err := auth.NewIssuer(ctx, s, strings.TrimRight(cfg.PublicURL, "/"))
	if err != nil {
		return err
	}
	policies := NewPolicyServer()
	z.OnReload(policies.Publish)
	if !slices.ContainsFunc(slices.Collect(maps.Keys(cfg.ServiceTokens)), func(name string) bool { return name != selfService }) {
		slog.Warn("OBSERVATORY_SERVICE_TOKENS is not set; the hub refuses every /internal/iam/ call")
	}
	mux.Handle("/internal/iam/", servicetoken.Middleware(cfg.ServiceTokens)(internalIAM(jwtIssuer, policies)))
	return nil
}

// internalIAM serves /internal/iam/*; the caller wraps it in servicetoken.Middleware.
func internalIAM(iss *auth.Issuer, ps *PolicyServer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/internal/iam/token", iss.TokenHandler())
	mux.Handle("/internal/iam/jwks", iss.JWKSHandler())
	path, h := iamv1connect.NewPolicyServiceHandler(ps)
	stream := http.StripPrefix("/internal/iam", h)
	mux.Handle("/internal/iam"+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The hub server's read/write timeouts would cut the long-lived stream.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
		stream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), streamControllerKey{}, rc)))
	}))
	return mux
}
