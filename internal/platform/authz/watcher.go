// Package authz authorizes service requests locally against the policy the hub streams.
package authz

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/google/uuid"
)

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

type orgUser struct{ org, user uuid.UUID }

type snapshot struct {
	revision uint64
	enforcer *coreauthz.Enforcer
	members  map[orgUser]bool
}

// Watcher keeps the latest valid policy snapshot from PolicyService.WatchPolicy.
type Watcher struct {
	client iamv1connect.PolicyServiceClient
	cur    atomic.Pointer[snapshot]
}

// NewWatcher streams from hubURL's /internal/iam/ with serviceToken; a nil client gets one without a timeout, which would cut the stream.
func NewWatcher(hubURL, serviceToken string, client *http.Client) (*Watcher, error) {
	if hubURL == "" || serviceToken == "" {
		return nil, errors.New("authz: hub url and service token are required")
	}
	if client == nil {
		client = &http.Client{}
	}
	base := strings.TrimSuffix(hubURL, "/") + "/internal/iam"
	return &Watcher{client: iamv1connect.NewPolicyServiceClient(client, base, connect.WithInterceptors(bearer(serviceToken)))}, nil
}

// bearer sets the service token on outgoing calls; WatchPolicy is a server stream, so the streaming client path matters.
type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Run watches until ctx ends, reconnecting with backoff; a lost stream keeps the last snapshot.
func (w *Watcher) Run(ctx context.Context) {
	backoff := minBackoff
	for {
		received, err := w.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		if received {
			backoff = minBackoff
		}
		slog.Warn("policy stream ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (w *Watcher) watch(ctx context.Context) (bool, error) {
	stream, err := w.client.WatchPolicy(ctx, connect.NewRequest(&iamv1.WatchPolicyRequest{}))
	if err != nil {
		return false, err
	}
	defer func() { _ = stream.Close() }()
	received := false
	for stream.Receive() {
		received = true
		w.apply(stream.Msg())
	}
	return received, stream.Err()
}

// apply swaps in a snapshot only if it builds an enforcer; otherwise the previous one stays.
func (w *Watcher) apply(m *iamv1.Policy) {
	p, err := policyFrom(m)
	var e *coreauthz.Enforcer
	if err == nil {
		e, err = coreauthz.New(p)
	}
	if err != nil {
		slog.Error("policy snapshot rejected, keeping the previous one", "revision", m.GetRevision(), "err", err)
		return
	}
	if cur := w.cur.Load(); cur != nil && m.GetRevision() < cur.revision {
		// Still applied: the stream is the only source, and ignoring it could freeze a revoked grant.
		slog.Warn("policy revision went backwards", "revision", m.GetRevision(), "previous", cur.revision)
	}
	members := make(map[orgUser]bool, len(p.Memberships))
	for _, mr := range p.Memberships {
		members[orgUser{mr.OrgID, mr.UserID}] = true
	}
	w.cur.Store(&snapshot{revision: m.GetRevision(), enforcer: e, members: members})
}

// Enforcer returns false until the first snapshot arrived.
func (w *Watcher) Enforcer() (*coreauthz.Enforcer, bool) {
	s := w.cur.Load()
	if s == nil {
		return nil, false
	}
	return s.enforcer, true
}

func policyFrom(m *iamv1.Policy) (coreauthz.Policy, error) {
	var p coreauthz.Policy
	var ids uuids
	for _, r := range m.GetMemberships() {
		p.Memberships = append(p.Memberships, coreauthz.MemberRole{OrgID: ids.parse(r.GetOrgId()), UserID: ids.parse(r.GetUserId()), Role: r.GetRole()})
	}
	for _, t := range m.GetTeamMembers() {
		p.TeamMembers = append(p.TeamMembers, coreauthz.TeamMember{OrgID: ids.parse(t.GetOrgId()), TeamID: ids.parse(t.GetTeamId()), UserID: ids.parse(t.GetUserId()), Admin: t.GetAdmin()})
	}
	for _, t := range m.GetTeams() {
		p.Teams = append(p.Teams, coreauthz.TeamRef{OrgID: ids.parse(t.GetOrgId()), ID: ids.parse(t.GetId())})
	}
	for _, r := range m.GetRoles() {
		role := coreauthz.CustomRole{ID: ids.parse(r.GetId()), OrgID: ids.parse(r.GetOrgId()), Name: r.GetName(), Description: r.GetDescription(), Parents: r.GetParents()}
		for _, rule := range r.GetRules() {
			role.Rules = append(role.Rules, coreauthz.Rule{Effect: coreauthz.Effect(rule.GetEffect()), Object: rule.GetObject(), Action: rule.GetAction()})
		}
		p.Roles = append(p.Roles, role)
	}
	for _, b := range m.GetBindings() {
		p.Bindings = append(p.Bindings, coreauthz.Binding{OrgID: ids.parse(b.GetOrgId()), Subject: b.GetSubject(), RoleID: ids.parse(b.GetRoleId())})
	}
	return p, ids.err
}

// uuids parses many canonical UUIDs and keeps the first error.
type uuids struct{ err error }

func (u *uuids) parse(s string) uuid.UUID {
	id, err := uuidx.Canonical(s)
	if err != nil && u.err == nil {
		u.err = err
	}
	return id
}
