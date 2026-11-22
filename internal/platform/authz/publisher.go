package authz

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
)

const policySendTimeout = 30 * time.Second

var (
	// keepaliveEvery resends the latest snapshot on a quiet stream, so watchers can tell silence from loss.
	keepaliveEvery = 30 * time.Second
)

type streamControllerKey struct{}

// Publisher streams the authorization snapshot; Publish never blocks, and a slow watcher skips to the latest snapshot.
type Publisher struct {
	mu     sync.Mutex
	rev    uint64
	last   *iamv1.Policy
	subs   map[chan struct{}]struct{}
	behind func() bool
}

func NewPublisher() *Publisher {
	// Seeding with the clock keeps revisions increasing across restarts, so watchers never see one go backwards.
	return &Publisher{rev: uint64(time.Now().UnixMicro()), subs: map[chan struct{}]struct{}{}}
}

// WithBehind sends nothing, keepalives and first snapshots included, while behind reports that the published policy may miss a commit: watchers then go stale and refuse instead of trusting it.
func (p *Publisher) WithBehind(behind func() bool) *Publisher {
	p.behind = behind
	return p
}

// Publish is fed from authz.Service.OnReload.
func (p *Publisher) Publish(pol coreauthz.Policy) {
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

func (p *Publisher) latest() *iamv1.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// Revision is the latest published revision, 0 before the first.
func (p *Publisher) Revision() uint64 {
	if l := p.latest(); l != nil {
		return l.Revision
	}
	return 0
}

func (p *Publisher) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch
}

func (p *Publisher) unsubscribe(ch chan struct{}) {
	p.mu.Lock()
	delete(p.subs, ch)
	p.mu.Unlock()
}

func (p *Publisher) WatchPolicy(ctx context.Context, _ *connect.Request[iamv1.WatchPolicyRequest], stream *connect.ServerStream[iamv1.Policy]) error {
	caller, _ := servicetoken.Caller(ctx)
	slog.Info("policy watch started", "caller", caller)
	defer slog.Info("policy watch ended", "caller", caller)
	ch := p.subscribe()
	defer p.unsubscribe(ch)
	keepalive := time.NewTicker(keepaliveEvery)
	defer keepalive.Stop()
	var sent uint64
	for {
		resend := false
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		case <-keepalive.C:
			resend = true
		}
		msg := p.latest()
		if msg == nil || p.behind != nil && p.behind() || msg.Revision == sent && !resend {
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

// Handler serves WatchPolicy under prefix (e.g. "/internal/iam") and returns the path to mount it on; the caller adds service-token auth.
func (p *Publisher) Handler(prefix string) (string, http.Handler) {
	path, h := iamv1connect.NewPolicyServiceHandler(p)
	stream := http.StripPrefix(prefix, h)
	return prefix + path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server's read/write timeouts would cut the long-lived stream.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
		stream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), streamControllerKey{}, rc)))
	})
}

func policyMessage(p coreauthz.Policy) *iamv1.Policy {
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
	for _, c := range p.Catalogs {
		m.Catalogs = append(m.Catalogs, catalog.ToProto(c))
	}
	return m
}
