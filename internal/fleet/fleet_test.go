package fleet_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/ssh"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

type env struct {
	t     *testing.T
	s     *store.Store
	z     *authz.Service
	f     *fleet.Fleet
	srv   *httptest.Server
	owner uuid.UUID
	org   store.Org
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(seal.Key{1})
	u, err := s.Login(ctx, store.Identity{Subject: "owner", Email: "owner@x.io", Name: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "A", u.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	f := fleet.New(s, z)
	srv := httptest.NewServer(http.HandlerFunc(f.ServeConnect))
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, z: z, f: f, srv: srv, owner: u.User.ID, org: org}
}

func (e *env) node(org uuid.UUID, name string) (store.Node, string) {
	e.t.Helper()
	ctx := context.Background()
	n, tok, err := e.s.CreateNode(ctx, store.Node{OrgID: org, Name: name, CreatedBy: e.owner})
	if err != nil {
		e.t.Fatal(err)
	}
	_, cred, err := e.s.EnrollNode(ctx, tok)
	if err != nil {
		e.t.Fatal(err)
	}
	return n, cred
}

func (e *env) vm(org, node uuid.UUID, name string, user, team *uuid.UUID) store.VM {
	e.t.Helper()
	v, err := e.s.CreateVM(context.Background(), store.VM{OrgID: org, NodeID: node, UserID: user, TeamID: team, Name: name, CPUs: 1, MemoryMB: 512, DiskGB: 5, CreatedBy: e.owner})
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *env) sshKey(user uuid.UUID) string {
	e.t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		e.t.Fatal(err)
	}
	k, err := e.s.AddSSHKey(context.Background(), store.SSHKey{UserID: user, Name: "k", PublicKey: string(ssh.MarshalAuthorizedKey(sp))})
	if err != nil {
		e.t.Fatal(err)
	}
	return k.PublicKey
}

func (e *env) envVar(name, value string) store.EnvVar {
	e.t.Helper()
	v, err := e.s.CreateEnvVar(context.Background(), store.EnvVar{OrgID: e.org.ID, Kind: store.EnvSecret, Scope: store.EnvScopeOrg, Name: name, CreatedBy: e.owner, UpdatedBy: e.owner}, value)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

type fakeNode struct {
	t    *testing.T
	sess *yamux.Session
	enc  *json.Encoder
	msgs chan proto.Message
}

func (e *env) dial(cred string) (*fakeNode, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http")
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}}})
	if err != nil {
		return nil, resp, err
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), cfg)
	if err != nil {
		return nil, resp, err
	}
	ctrl, err := sess.Open()
	if err != nil {
		return nil, resp, err
	}
	n := &fakeNode{t: e.t, sess: sess, enc: json.NewEncoder(ctrl), msgs: make(chan proto.Message, 16)}
	go func() {
		defer close(n.msgs)
		dec := json.NewDecoder(ctrl)
		for {
			var m proto.Message
			if dec.Decode(&m) != nil {
				return
			}
			n.msgs <- m
		}
	}()
	e.t.Cleanup(func() { sess.Close() })
	return n, resp, nil
}

func (e *env) connect(cred string, vms ...proto.VMStatus) *fakeNode {
	e.t.Helper()
	n, _, err := e.dial(cred)
	if err != nil {
		e.t.Fatal(err)
	}
	n.send(proto.Message{Type: "register", Register: &proto.Register{NodeID: "whatever", Hostname: "h", CPUs: 4, MemoryMB: 4096, VMs: vms, Version: "v1"}})
	return n
}

func (n *fakeNode) send(m proto.Message) {
	n.t.Helper()
	if err := n.enc.Encode(m); err != nil {
		n.t.Fatal(err)
	}
}

func (n *fakeNode) desired() proto.Message {
	n.t.Helper()
	select {
	case m, ok := <-n.msgs:
		if !ok {
			n.t.Fatal("connection closed")
		}
		if m.Type != "desired" {
			n.t.Fatalf("got %q message", m.Type)
		}
		for _, d := range m.Desired {
			if d.Boot == nil || d.Boot.SSHPublicKeys == nil || d.Boot.Env == nil {
				n.t.Fatalf("incomplete boot for %s: %+v", d.Spec.ID, d.Boot)
			}
		}
		return m
	case <-time.After(5 * time.Second):
		n.t.Fatal("no desired list")
	}
	return proto.Message{}
}

func (n *fakeNode) closed() bool {
	select {
	case _, ok := <-n.msgs:
		return !ok
	case <-time.After(5 * time.Second):
		return false
	}
}

func ids(m proto.Message) []string {
	var out []string
	for _, d := range m.Desired {
		out = append(out, d.Spec.ID)
	}
	slices.Sort(out)
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met")
}

func TestBadCredential(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n1")
	for _, c := range []string{"", "garbage", n.ID.String() + ".wrong", uuid.NewString() + ".x"} {
		if _, resp, err := e.dial(c); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("credential %q: err=%v resp=%v", c, err, resp)
		}
	}
	if _, err := e.s.RegenerateNodeCredential(context.Background(), e.org.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, resp, err := e.dial(cred); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked credential: err=%v resp=%v", err, resp)
	}
}

func TestDesiredOnlyOwnNodeAndForeignStatusIgnored(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	other, err := e.s.CreateOrg(ctx, "B", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	a, cred := e.node(e.org.ID, "a")
	a2, _ := e.node(e.org.ID, "a2")
	b, _ := e.node(other.ID, "b")
	mine := e.vm(e.org.ID, a.ID, "mine", &e.owner, nil)
	sibling := e.vm(e.org.ID, a2.ID, "sibling", &e.owner, nil)
	foreign := e.vm(other.ID, b.ID, "foreign", &e.owner, nil)

	// The node also reports a VM the hub doesn't place on it: never written anywhere.
	node := e.connect(cred, proto.VMStatus{ID: foreign.ID.String(), State: proto.StateRunning, IP: "1.2.3.4"})
	m := node.desired()
	if got := ids(m); !slices.Equal(got, []string{mine.ID.String()}) || m.Seq == 0 {
		t.Fatalf("desired = %v seq %d", got, m.Seq)
	}
	d := m.Desired[0]
	if d.Spec.Name != "mine" || d.Spec.CPUs != 1 || d.Spec.MemoryMB != 512 || d.Spec.DiskGB != 5 || d.Power != mine.Power || d.Generation != mine.Generation {
		t.Errorf("unexpected desired VM %+v", d)
	}
	if !e.f.Online(a.ID) || e.f.Online(a2.ID) {
		t.Error("wrong online state")
	}
	info, err := e.s.Node(ctx, e.org.ID, a.ID)
	if err != nil || info.CPUs != 4 || info.MemoryMB != 4096 || info.Version != "v1" || info.LastSeen == nil {
		t.Errorf("node info not updated: %+v %v", info, err)
	}

	for _, v := range []store.VM{foreign, sibling, mine} {
		node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning, IP: "10.0.0.9"}})
	}
	waitFor(t, func() bool {
		v, err := e.s.VM(ctx, e.org.ID, mine.ID)
		return err == nil && v.State == proto.StateRunning && v.IP == "10.0.0.9" && v.Provisioned
	})
	for _, v := range []store.VM{foreign, sibling} {
		got, err := e.s.VM(ctx, v.OrgID, v.ID)
		if err != nil || got.State != v.State || got.IP != "" || got.Provisioned {
			t.Errorf("status of %s applied: %+v %v", v.Name, got, err)
		}
	}
}

func TestTeamMemberRemovalDropsKey(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	team, err := e.s.CreateTeam(ctx, e.org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := e.s.Login(ctx, store.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.s.CreateInvite(ctx, e.org.ID, store.RoleMember, e.owner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.AcceptInvite(ctx, tok, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []uuid.UUID{e.owner, bob.User.ID} {
		if err := e.s.SetTeamMember(ctx, e.org.ID, team.ID, u, false); err != nil {
			t.Fatal(err)
		}
	}
	ownerKey, bobKey := e.sshKey(e.owner), e.sshKey(bob.User.ID)
	e.vm(e.org.ID, n.ID, "team-vm", nil, &team.ID)

	node := e.connect(cred)
	first := node.desired()
	if keys := first.Desired[0].Boot.SSHPublicKeys; len(keys) != 2 || !slices.Contains(keys, ownerKey) || !slices.Contains(keys, bobKey) {
		t.Fatalf("keys = %v", keys)
	}
	if err := e.s.RemoveTeamMember(ctx, e.org.ID, team.ID, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	second := node.desired()
	if keys := second.Desired[0].Boot.SSHPublicKeys; !slices.Equal(keys, []string{ownerKey}) {
		t.Fatalf("keys after removal = %v", keys)
	}
	if second.Seq <= first.Seq {
		t.Errorf("seq did not increase: %d then %d", first.Seq, second.Seq)
	}
}

func TestDeletedVariableDropped(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	gone := e.envVar("GONE", "g")
	e.envVar("KEPT", "k")

	node := e.connect(cred)
	if env := node.desired().Desired[0].Boot.Env; env["GONE"] != "g" || env["KEPT"] != "k" {
		t.Fatalf("env = %v", env)
	}
	if err := e.s.DeleteEnvVar(ctx, gone); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	if env := node.desired().Desired[0].Boot.Env; len(env) != 1 || env["KEPT"] != "k" {
		t.Fatalf("env after delete = %v", env)
	}
}

func TestDesiredRetriedAfterBuildFailure(t *testing.T) {
	old := *fleet.DesiredRetry
	*fleet.DesiredRetry = 50 * time.Millisecond
	t.Cleanup(func() { *fleet.DesiredRetry = old })
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	node := e.connect(cred)
	first := node.desired()
	// Sealed under another master key, the variable makes building the list fail until it is deleted.
	bad, err := e.s.WithMasterKey(seal.Key{2}).CreateEnvVar(ctx, store.EnvVar{OrgID: e.org.ID, Kind: store.EnvSecret, Scope: store.EnvScopeOrg, Name: "BAD", CreatedBy: e.owner, UpdatedBy: e.owner}, "x")
	if err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	time.Sleep(100 * time.Millisecond)
	if err := e.s.DeleteEnvVar(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if m := node.desired(); m.Seq <= first.Seq {
		t.Fatalf("seq %d after %d", m.Seq, first.Seq)
	}
}

func TestVMDeletedWhileOfflineAbsentOnReconnect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	keep := e.vm(e.org.ID, n.ID, "keep", &e.owner, nil)
	drop := e.vm(e.org.ID, n.ID, "drop", &e.owner, nil)

	node := e.connect(cred)
	if got := ids(node.desired()); len(got) != 2 {
		t.Fatalf("desired = %v", got)
	}
	node.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(n.ID) })
	if err := e.s.DeleteVM(ctx, e.org.ID, drop.ID); err != nil {
		t.Fatal(err)
	}
	node = e.connect(cred, proto.VMStatus{ID: keep.ID.String(), State: proto.StateStopped}, proto.VMStatus{ID: drop.ID.String(), State: proto.StateRunning})
	m := node.desired()
	if got := ids(m); !slices.Equal(got, []string{keep.ID.String()}) {
		t.Fatalf("desired after reconnect = %v", got)
	}
	if !m.Desired[0].Provisioned {
		t.Error("register status should mark the VM provisioned before the first list")
	}
}

func TestReplaceAndDisconnect(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	first := e.connect(cred)
	first.desired()
	second := e.connect(cred)
	second.desired()
	if !first.closed() {
		t.Fatal("first connection not closed on replacement")
	}
	if !e.f.Online(n.ID) {
		t.Fatal("replacement not online")
	}
	e.f.Disconnect(n.ID)
	if !second.closed() || e.f.Online(n.ID) {
		t.Fatal("Disconnect did not drop the node")
	}
	// A slow or gone node never blocks NotifyOrg.
	e.f.NotifyOrg(e.org.ID)
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNoSecretsInLogs(t *testing.T) {
	ctx := context.Background()
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	key := e.sshKey(e.owner)
	e.envVar("TOKEN", "s3cr3t-value")

	node := e.connect(cred)
	node.desired()
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning}})
	e.f.NotifyOrg(e.org.ID)
	node.desired()
	if _, err := e.s.RegenerateNodeCredential(ctx, e.org.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	e.f.Disconnect(n.ID)
	node.closed()
	waitFor(t, func() bool { return strings.Contains(buf.String(), "node disconnected") })

	out := buf.String()
	if !strings.Contains(out, "node connected") {
		t.Fatalf("log capture broken: %q", out)
	}
	keyBody := strings.Fields(key)[1]
	for _, secret := range []string{"s3cr3t-value", "TOKEN", keyBody, cred} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
}

func TestTeamVMNeverGetsPrivateVariable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	team, err := e.s.CreateTeam(ctx, e.org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetTeamMember(ctx, e.org.ID, team.ID, e.owner, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateEnvVar(ctx, store.EnvVar{OrgID: e.org.ID, Kind: store.EnvSecret, Scope: store.EnvScopeUser, OwnerID: &e.owner, Name: "PRIVATE", CreatedBy: e.owner, UpdatedBy: e.owner}, "mine-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateEnvVar(ctx, store.EnvVar{OrgID: e.org.ID, Kind: store.EnvSecret, Scope: store.EnvScopeTeam, OwnerID: &team.ID, Name: "TEAM", CreatedBy: e.owner, UpdatedBy: e.owner}, "shared"); err != nil {
		t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	teamVM := e.vm(e.org.ID, n.ID, "team-vm", nil, &team.ID)
	userVM := e.vm(e.org.ID, n.ID, "user-vm", &e.owner, nil)

	for _, d := range e.connect(cred).desired().Desired {
		switch d.Spec.ID {
		case teamVM.ID.String():
			if _, ok := d.Boot.Env["PRIVATE"]; ok || d.Boot.Env["TEAM"] != "shared" {
				t.Errorf("team VM env = %v", d.Boot.Env)
			}
		case userVM.ID.String():
			if d.Boot.Env["PRIVATE"] != "mine-only" {
				t.Errorf("user VM env = %v", d.Boot.Env)
			}
		}
	}
}
