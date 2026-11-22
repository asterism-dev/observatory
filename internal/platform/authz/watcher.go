// Package authz authorizes service requests locally against the policy iam streams.
package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/google/uuid"
)

const (
	minBackoff = time.Second
	maxBackoff = 5 * time.Second
	// maxStale matches the JWT lifetime, so a lost stream opens no revocation window beyond it.
	maxStale = 5 * time.Minute
)

// silentAfter is three missed keepalives: a half-open stream errs on nothing, so silence counts as loss.
var silentAfter = 3 * keepaliveEvery

type snapshot struct {
	revision uint64
	policy   coreauthz.Policy
	enforcer *coreauthz.Enforcer
	// digests maps each catalog's service to its digest.
	digests map[string]string
}

// Watcher keeps the latest valid policy snapshot from PolicyService.WatchPolicy.
type Watcher struct {
	client iamv1connect.PolicyServiceClient
	cur    atomic.Pointer[snapshot]
	now    func() time.Time
	// downSince is when the stream was lost, in Unix nanoseconds; 0 while it delivers.
	downSince atomic.Int64
	lastSeen  atomic.Int64
	// required maps a service to the catalog digest Current waits for; replaced, never mutated.
	required atomic.Pointer[map[string]string]

	mu       sync.Mutex
	changed  chan struct{} // closed and replaced on every applied snapshot
	onChange func()
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
	return &Watcher{
		client:  iamv1connect.NewPolicyServiceClient(client, base, connect.WithInterceptors(servicetoken.Bearer(serviceToken))),
		now:     time.Now,
		changed: make(chan struct{}),
	}, nil
}

// OnChange sets fn to run after every applied snapshot, on the watching goroutine.
func (w *Watcher) OnChange(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onChange = fn
}

// Run watches until ctx ends, reconnecting with backoff; a lost stream keeps the last snapshot, which Current refuses after maxStale.
func (w *Watcher) Run(ctx context.Context) {
	backoff := minBackoff
	for {
		received, err := w.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		w.downSince.CompareAndSwap(0, w.now().UnixNano())
		if received {
			backoff = minBackoff
		}
		slog.Warn("policy stream ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
}

func nextBackoff(d time.Duration) time.Duration { return min(d*2, maxBackoff) }

func (w *Watcher) watch(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	silent := time.AfterFunc(silentAfter, cancel)
	defer silent.Stop()
	stream, err := w.client.WatchPolicy(ctx, connect.NewRequest(&iamv1.WatchPolicyRequest{}))
	if err != nil {
		return false, err
	}
	defer func() { _ = stream.Close() }()
	received := false
	for stream.Receive() {
		received = true
		if !w.apply(stream.Msg()) {
			w.downSince.CompareAndSwap(0, w.now().UnixNano())
			continue
		}
		w.downSince.Store(0)
		w.lastSeen.Store(w.now().UnixNano())
		silent.Reset(silentAfter)
	}
	return received, stream.Err()
}

// apply swaps in a snapshot only if it builds an enforcer; false leaves the watcher as blind as a lost stream, and a repeat of the current revision is a keepalive.
func (w *Watcher) apply(m *iamv1.Policy) bool {
	if cur := w.cur.Load(); cur != nil && m.GetRevision() == cur.revision {
		return true
	}
	p, err := policyFrom(m)
	p.Revision = m.GetRevision()
	var e *coreauthz.Enforcer
	if err == nil {
		e, err = coreauthz.New(p)
	}
	if err != nil {
		slog.Error("policy snapshot rejected, keeping the previous one", "revision", m.GetRevision(), "err", err)
		return false
	}
	if cur := w.cur.Load(); cur != nil && m.GetRevision() < cur.revision {
		// Still applied: the stream is the only source, and ignoring it could freeze a revoked grant.
		slog.Warn("policy revision went backwards", "revision", m.GetRevision(), "previous", cur.revision)
	}
	digests := make(map[string]string, len(p.Catalogs))
	for _, c := range p.Catalogs {
		digests[c.Service] = catalog.Digest(c)
	}
	w.cur.Store(&snapshot{revision: m.GetRevision(), policy: p, enforcer: e, digests: digests})
	w.mu.Lock()
	close(w.changed)
	w.changed = make(chan struct{})
	fn := w.onChange
	w.mu.Unlock()
	if fn != nil {
		fn()
	}
	return true
}

// Require makes Current return false until the snapshot carries service's catalog with digest.
func (w *Watcher) Require(service, digest string) {
	// ponytail: one digest per service, so instances on different releases flip each other unavailable in a rolling deploy; key by release when services run replicas.
	w.mu.Lock()
	defer w.mu.Unlock()
	next := map[string]string{service: digest}
	if cur := w.required.Load(); cur != nil {
		for k, v := range *cur {
			if k != service {
				next[k] = v
			}
		}
	}
	w.required.Store(&next)
}

// Current returns false until the first snapshot with every required catalog digest arrived, and once the stream has been down, or silent past silentAfter, for longer than maxStale.
func (w *Watcher) Current() (*coreauthz.Enforcer, bool) {
	s := w.cur.Load()
	if s == nil {
		return nil, false
	}
	now := w.now()
	if down := w.downSince.Load(); down != 0 && now.Sub(time.Unix(0, down)) > maxStale {
		return nil, false
	}
	if seen := w.lastSeen.Load(); seen != 0 && now.Sub(time.Unix(0, seen)) > silentAfter+maxStale {
		return nil, false
	}
	if req := w.required.Load(); req != nil {
		for svc, d := range *req {
			if s.digests[svc] != d {
				return nil, false
			}
		}
	}
	return s.enforcer, true
}

// WithClock replaces the clock staleness is judged by; for tests.
func (w *Watcher) WithClock(now func() time.Time) *Watcher {
	w.now = now
	return w
}

// Policy returns the latest snapshot and its revision, however old; false until the first arrived.
func (w *Watcher) Policy() (coreauthz.Policy, uint64, bool) {
	s := w.cur.Load()
	if s == nil {
		return coreauthz.Policy{}, 0, false
	}
	return s.policy, s.revision, true
}

// WaitRevision returns once a snapshot at rev or later is applied, or with ctx's error; rev 0 waits for the first snapshot.
func (w *Watcher) WaitRevision(ctx context.Context, rev uint64) error {
	for {
		w.mu.Lock()
		ch := w.changed
		w.mu.Unlock()
		if s := w.cur.Load(); s != nil && s.revision >= rev {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
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
	for _, pc := range m.GetCatalogs() {
		// ponytail: one undecodable catalog (a newer service's relation) rejects the whole snapshot; skip just that catalog once services ship independently.
		c, err := catalog.FromProto(pc)
		if err != nil {
			return p, fmt.Errorf("catalog %s: %w", pc.GetService(), err)
		}
		p.Catalogs = append(p.Catalogs, c)
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
