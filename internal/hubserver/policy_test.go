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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
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

// streamEnv is a store with bob owning a VM in acme, and a publisher streaming iam's policy of it.
type streamEnv struct {
	s   *store.Store
	pub *platformauthz.Publisher
	org uuid.UUID
	bob uuid.UUID
	vm  authz.VMRef
	w   *platformauthz.Watcher
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(seal.Key{1})
	login := func(sub string) uuid.UUID {
		res, err := s.Login(ctx, store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
		if err != nil {
			t.Fatal(err)
		}
		return res.User.ID
	}
	alice, bob := login("alice"), login("bob")
	org, err := s.CreateOrg(ctx, "acme", alice)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(ctx, org.ID, store.RoleMember, alice, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, inv, bob); err != nil {
		t.Fatal(err)
	}
	node, enroll, err := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n1", CreatedBy: alice})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollNode(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	vm, err := s.CreateVM(ctx, store.VM{OrgID: org.ID, NodeID: node.ID, UserID: &bob, Name: "bobvm", CPUs: 1, MemoryMB: 128, DiskGB: 1, CreatedBy: bob})
	if err != nil {
		t.Fatal(err)
	}
	e := &streamEnv{s: s, pub: platformauthz.NewPublisher(), org: org.ID, bob: bob, vm: authz.VMRef{OrgID: org.ID, ID: vm.ID, UserID: &bob}}
	e.publish(t, func(*authz.Policy) {})
	mux := http.NewServeMux()
	mux.Handle(e.pub.Handler("/internal/iam"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if e.w, err = platformauthz.NewWatcher(srv.URL, svcToken, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

// publish streams iam's committed policy, as iam would after a commit, with change applied on top.
func (e *streamEnv) publish(t *testing.T, change func(*authz.Policy)) {
	t.Helper()
	p, err := e.s.PolicySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	change(&p)
	e.pub.Publish(p)
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
	other, team := uuid.New(), uuid.New()
	e.publish(t, func(p *authz.Policy) { p.Teams = append(p.Teams, authz.TeamRef{OrgID: other, ID: team}) })
	waitUntil(t, "the unrelated update", func() bool { return hp.Enforcer().HasTeam(other, team) })
	if !hp.Enforcer().CanConnect(e.bob, e.vm) {
		t.Fatal("the VM owner lost access on an unrelated policy message")
	}
	if _, ok := hp.Current(); !ok {
		t.Fatal("hub policy not current with a live stream")
	}

	e.publish(t, func(p *authz.Policy) {
		p.Memberships = slices.DeleteFunc(p.Memberships, func(m authz.MemberRole) bool { return m.UserID == e.bob })
	})
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
	if err := hp.WaitRevision(short, e.pub.Revision()+1); err == nil {
		t.Fatal("WaitRevision returned before iam published that revision")
	}
	team := uuid.New()
	e.publish(t, func(p *authz.Policy) { p.Teams = append(p.Teams, authz.TeamRef{OrgID: e.org, ID: team}) })
	if err := hp.WaitRevision(t.Context(), e.pub.Revision()); err != nil {
		t.Fatal(err)
	}
	// WaitRevision promises the enforcer itself includes the revision, not just the watcher.
	if !hp.Enforcer().HasTeam(e.org, team) {
		t.Fatal("enforcer behind the revision WaitRevision returned for")
	}
}
