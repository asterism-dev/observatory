package hubserver

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

// testHubEnv is the minimal environment Run starts with; the bus may be unreachable.
func testHubEnv(t *testing.T, dbURL, natsURL, natsToken string) map[string]string {
	t.Helper()
	idp := httptest.NewServer(nil)
	t.Cleanup(idp.Close)
	idp.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issuer":"`+idp.URL+`","authorization_endpoint":"`+idp.URL+`/auth","token_endpoint":"`+idp.URL+`/token","jwks_uri":"`+idp.URL+`/keys"}`)
	})
	return map[string]string{
		"OBSERVATORY_DATABASE_URL":       dbURL,
		"OBSERVATORY_PUBLIC_URL":         "http://obs.example",
		"OBSERVATORY_OIDC_ISSUER":        idp.URL,
		"OBSERVATORY_OIDC_WEB_CLIENT_ID": "web",
		"OBSERVATORY_OIDC_CLI_CLIENT_ID": "cli",
		"OBSERVATORY_GATEWAY_TOKEN":      strings.Repeat("g", 32),
		"OBSERVATORY_STORAGE_TOKEN":      strings.Repeat("s", 32),
		"OBSERVATORY_EDGE_TOKEN":         strings.Repeat("e", 32),
		"OBSERVATORY_SECRETS_KEY":        base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"OBSERVATORY_NO_UPDATE_CHECK":    "1",
		"OBSERVATORY_NODE_BINARY":        t.TempDir() + "/none",
		"OBSERVATORY_NATS_URL":           natsURL,
		"OBSERVATORY_NATS_TOKEN":         natsToken,
	}
}

func TestHubWaitsForFirstSnapshot(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	dead.Close()
	prevURL, prevWait := iamURL, firstPolicyWait
	iamURL, firstPolicyWait = func(net.Listener) string { return deadURL }, 300*time.Millisecond
	defer func() { iamURL, firstPolicyWait = prevURL, prevWait }()

	pool := dbtest.New(t)
	env := testHubEnv(t, pool.Config().ConnString(), "nats://"+dead.Addr().String(), strings.Repeat("n", 32))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- Run(t.Context(), func(k string) string { return env[k] }, ln) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "policy") {
			t.Fatalf("Run: %v, want a policy error", err)
		}
		if time.Since(start) < firstPolicyWait {
			t.Fatalf("Run failed after %v, before the wait", time.Since(start))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not give up on the policy stream")
	}
}

func TestChangedOrgs(t *testing.T) {
	a, b, user, team, role := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	base := func() authz.Policy {
		return authz.Policy{
			Memberships: []authz.MemberRole{{OrgID: a, UserID: user, Role: "member"}, {OrgID: b, UserID: user, Role: "member"}},
			Teams:       []authz.TeamRef{{OrgID: a, ID: team}},
			TeamMembers: []authz.TeamMember{{OrgID: a, TeamID: team, UserID: user}},
			Roles:       []authz.CustomRole{{ID: role, OrgID: b, Name: "r", Rules: []authz.Rule{{Effect: authz.Allow, Object: "org/vms/*", Action: "compute.instances.get"}}}},
		}
	}
	for _, c := range []struct {
		name   string
		change func(*authz.Policy)
		want   []uuid.UUID
	}{
		{"unchanged", func(*authz.Policy) {}, nil},
		{"owner refs only", func(p *authz.Policy) { p.VMs = []authz.VMRef{{OrgID: a, ID: uuid.New(), UserID: &user}} }, nil},
		{"order only", func(p *authz.Policy) { slices.Reverse(p.Memberships) }, nil},
		{"role change", func(p *authz.Policy) { p.Memberships[0].Role = "admin" }, []uuid.UUID{a}},
		{"member removed", func(p *authz.Policy) { p.Memberships = p.Memberships[1:] }, []uuid.UUID{a}},
		{"team admin", func(p *authz.Policy) { p.TeamMembers[0].Admin = true }, []uuid.UUID{a}},
		{"team added", func(p *authz.Policy) { p.Teams = append(p.Teams, authz.TeamRef{OrgID: b, ID: uuid.New()}) }, []uuid.UUID{b}},
		{"custom role rule", func(p *authz.Policy) { p.Roles[0].Rules[0].Action = "compute.instances.setPower" }, []uuid.UUID{b}},
		{"binding", func(p *authz.Policy) {
			p.Bindings = append(p.Bindings, authz.Binding{OrgID: b, Subject: authz.UserSubject(user), RoleID: role})
		}, []uuid.UUID{b}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.change(&p)
			if got := changedOrgs(base(), p); !slices.Equal(got, c.want) {
				t.Fatalf("changedOrgs = %v, want %v", got, c.want)
			}
		})
	}
}

// streamEnv is a store with bob owning a VM in acme, and iam streaming the policy of it.
type streamEnv struct {
	s   *store.Store
	iam *iamtest.Server
	org uuid.UUID
	bob uuid.UUID
	vm  authz.VMRef
	w   *platformauthz.Watcher
	// clock is the watcher's, in Unix nanoseconds.
	clock atomic.Int64
	pool  *pgxpool.Pool
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(seal.Key{1})
	iam := iamtest.New(t)
	alice, bob := iam.AddUser("alice").ID, iam.AddUser("bob").ID
	org := iam.CreateOrg(alice, "acme")
	iam.AddMember(org, bob, "member")
	node, enroll, err := s.CreateNode(ctx, store.Node{OrgID: org, Name: "n1", CreatedBy: alice})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollNode(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	vm, err := s.CreateVM(ctx, store.VM{OrgID: org, NodeID: node.ID, UserID: &bob, Name: "bobvm", CPUs: 1, MemoryMB: 128, DiskGB: 1, CreatedBy: bob})
	if err != nil {
		t.Fatal(err)
	}
	e := &streamEnv{s: s, iam: iam, org: org, bob: bob, vm: authz.VMRef{OrgID: org, ID: vm.ID, UserID: &bob}, pool: pool}
	e.clock.Store(time.Now().UnixNano())
	if e.w, err = platformauthz.NewWatcher(iam.URL(), iam.ServiceToken(), nil); err != nil {
		t.Fatal(err)
	}
	e.w.WithClock(func() time.Time { return time.Unix(0, e.clock.Load()) })
	return e
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOwnerGrantSurvivesStreamUpdate(t *testing.T) {
	e := newStreamEnv(t)
	hp, err := watchPolicy(t.Context(), e.w, e.s)
	if err != nil {
		t.Fatal(err)
	}
	if !hp.Enforcer().CanConnect(e.bob, e.vm) {
		t.Fatal("the VM owner cannot connect after the first snapshot")
	}
	other := uuid.New()
	team := e.iam.AddTeam(other, "core")
	waitUntil(t, "the unrelated update", func() bool { return hp.Enforcer().HasTeam(other, team) })
	if !hp.Enforcer().CanConnect(e.bob, e.vm) {
		t.Fatal("the VM owner lost access on an unrelated policy message")
	}
	if _, ok := hp.Current(); !ok {
		t.Fatal("hub policy not current with a live stream")
	}

	e.iam.RemoveMember(e.org, e.bob)
	waitUntil(t, "bob's removal", func() bool { _, ok := hp.Enforcer().Role(e.org, e.bob); return !ok })
}

func TestHubCanHoldWaitsForStreamRevision(t *testing.T) {
	e := newStreamEnv(t)
	hp, err := watchPolicy(t.Context(), e.w, e.s)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := hp.WaitRevision(short, e.iam.Revision()+1); err == nil {
		t.Fatal("WaitRevision returned before iam published that revision")
	}
	team := e.iam.AddTeam(e.org, "core")
	if err := hp.WaitRevision(t.Context(), e.iam.Revision()); err != nil {
		t.Fatal(err)
	}
	// WaitRevision promises the enforcer itself includes the revision, not just the watcher.
	if !hp.Enforcer().HasTeam(e.org, team) {
		t.Fatal("enforcer behind the revision WaitRevision returned for")
	}
}

func TestHubPolicyFreshness(t *testing.T) {
	e := newStreamEnv(t)
	hp, err := watchPolicy(t.Context(), e.w, e.s)
	if err != nil {
		t.Fatal(err)
	}
	bobIn := func() bool { _, ok := hp.Enforcer().Role(e.org, e.bob); return ok }
	fresh := func() bool { _, ok := hp.Current(); return ok }
	if !fresh() || !bobIn() {
		t.Fatal("not current after the first snapshot")
	}

	// A reload that fails after a stream message leaves the hub behind iam: it refuses until a retry succeeds.
	if _, err := e.pool.Exec(t.Context(), "ALTER TABLE vms RENAME TO vms_away"); err != nil {
		t.Fatal(err)
	}
	e.iam.AddTeam(e.org, "core")
	waitUntil(t, "the failed reload to withhold the policy", func() bool { return !fresh() && !bobIn() })
	if _, err := e.pool.Exec(t.Context(), "ALTER TABLE vms_away RENAME TO vms"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the retried reload", func() bool { return fresh() && bobIn() })

	// A stream silent past the keepalive window and maxStale withholds it too.
	e.clock.Add(int64(6*time.Minute + 31*time.Second))
	if fresh() || bobIn() {
		t.Fatal("still current on a silent stream")
	}
	e.iam.AddTeam(e.org, "core")
	waitUntil(t, "the next message to make the stream live", fresh)
}
