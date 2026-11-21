package fleet_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	s     *store.Store
	z     *authz.Service
	h     *network.Hub
	f     *fleet.Fleet
	srv   *httptest.Server
	owner uuid.UUID
	org   store.Org
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(seal.Key{1})
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
	h, err := network.Load(ctx, s, z, network.Config{GatewayHost: "gw.test", GatewayToken: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.New(ctx, s, z, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	srv := httptest.NewServer(http.HandlerFunc(f.ServeConnect))
	t.Cleanup(srv.Close)
	return &env{t: t, pool: pool, s: s, z: z, h: h, f: f, srv: srv, owner: u.User.ID, org: org}
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
	return e.register(cred, proto.Register{VMs: vms})
}

// register fills in the fixed node facts; NodeID is deliberately bogus, the hub must use the credential.
func (e *env) register(cred string, reg proto.Register) *fakeNode {
	e.t.Helper()
	n, _, err := e.dial(cred)
	if err != nil {
		e.t.Fatal(err)
	}
	reg.NodeID, reg.Hostname, reg.CPUs, reg.MemoryMB = "whatever", "h", 4, 4096
	if reg.Version == "" {
		reg.Version = "v1"
	}
	n.send(proto.Message{Type: "register", Register: &reg})
	return n
}

func (n *fakeNode) send(m proto.Message) {
	n.t.Helper()
	if err := n.enc.Encode(m); err != nil {
		n.t.Fatal(err)
	}
}

func (n *fakeNode) next() proto.Message {
	n.t.Helper()
	select {
	case m, ok := <-n.msgs:
		if !ok {
			n.t.Fatal("connection closed")
		}
		return m
	case <-time.After(5 * time.Second):
		n.t.Fatal("no message")
	}
	return proto.Message{}
}

// desired returns the next desired message.
func (n *fakeNode) desired() proto.Message {
	n.t.Helper()
	for {
		m := n.next()
		if m.Type != "desired" {
			n.t.Fatalf("got %q message", m.Type)
		}
		for _, d := range m.Desired {
			if d.Boot == nil || d.Boot.SSHPublicKeys == nil || d.Boot.Env == nil {
				n.t.Fatalf("incomplete boot for %s: %+v", d.Spec.ID, d.Boot)
			}
		}
		return m
	}
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

	if _, err := e.s.SetAutoSuspend(ctx, e.org.ID, mine.ID, 15); err != nil {
		t.Fatal(err)
	}
	// The node also reports a VM the hub doesn't place on it: never written anywhere.
	node := e.connect(cred, proto.VMStatus{ID: foreign.ID.String(), State: proto.StateRunning, IP: "1.2.3.4"})
	m := node.desired()
	if got := ids(m); !slices.Equal(got, []string{mine.ID.String()}) || m.Seq == 0 {
		t.Fatalf("desired = %v seq %d", got, m.Seq)
	}
	d := m.Desired[0]
	if d.Spec.Name != "mine" || d.Spec.CPUs != 1 || d.Spec.MemoryMB != 512 || d.Spec.DiskGB != 5 || d.Power != mine.Power || d.Generation != mine.Generation || d.AutoSuspendMinutes != 15 || d.WakeSeq != 0 {
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
	if err := e.s.DeleteVM(ctx, e.org.ID, drop.ID, nil, store.DeleteReasonUser); err != nil {
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

	e.readyDisk(v, "data")

	node := e.connect(cred)
	psk := node.desired().Desired[0].Spec.Attachments[0].PSK
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning}})
	s := e.detach(node, v)
	node.send(proto.Message{Type: "disks", AppliedSeq: s})
	waitFor(t, func() bool { return strings.Contains(buf.String(), "disk release confirmed") })
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
	for _, secret := range []string{"s3cr3t-value", "TOKEN", keyBody, cred, psk, "nbd://"} {
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

func TestDesiredCarriesNICs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	if !v.MgmtIP.IsValid() {
		t.Fatal("vm has no management address")
	}
	node := e.connect(cred)
	first := node.desired().Desired[0]
	want := []proto.NIC{{Network: "mgmt", MAC: netaddr.MAC(v.MgmtIP), IP: v.MgmtIP.String(), Port: v.ID.String() + "-mgmt"}}
	if !slices.Equal(first.Spec.NICs, want) {
		t.Fatalf("NICs = %+v, want %+v", first.Spec.NICs, want)
	}

	zeta, err := e.s.CreateNetwork(ctx, store.Network{OrgID: e.org.ID, Name: "zeta", CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := e.s.CreateNetwork(ctx, store.Network{OrgID: e.org.ID, Name: "alpha", CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, v.ID, []uuid.UUID{zeta.ID, alpha.ID}, true); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	second := node.desired().Desired[0]
	if second.Generation <= first.Generation {
		t.Errorf("generation %d after %d", second.Generation, first.Generation)
	}
	nics := second.Spec.NICs
	if len(nics) != 3 || nics[0] != want[0] || nics[1].Network != alpha.ID.String() || nics[2].Network != zeta.ID.String() {
		t.Fatalf("NICs = %+v", nics)
	}

	// Every NIC must bind to exactly the port the gateway creates for it.
	st, err := e.h.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ports := map[string]proto.GatewayPort{}
	for _, o := range st.Orgs {
		for _, p := range o.Ports {
			ports[p.Name] = p
		}
		for _, gn := range o.Networks {
			for _, p := range gn.Ports {
				ports[p.Name] = p
			}
		}
	}
	for _, nic := range nics {
		p, ok := ports[nic.Port]
		if !ok || p.MAC != nic.MAC || p.IP != nic.IP || p.Chassis != n.ID.String() {
			t.Errorf("NIC %+v has gateway port %+v (found %v)", nic, p, ok)
		}
	}
}

// readyDisk creates a disk of the owner that the storage server already has, attached to v at /data.
func (e *env) readyDisk(v store.VM, name string) store.Disk {
	e.t.Helper()
	ctx := context.Background()
	d, err := e.s.CreateDisk(ctx, store.Disk{OrgID: v.OrgID, UserID: v.UserID, TeamID: v.TeamID, Name: name, SizeGB: 1, CreatedBy: e.owner})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ApplyStorageFiles(ctx, []proto.StorageFile{{Name: proto.StorageFileName(d.ID.String()), SizeBytes: 1 << 30}}); err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.s.SetVMDisks(ctx, v.OrgID, v.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, true); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// releaseSeq returns the export's release_seq; ok is false once the export is gone.
func (e *env) releaseSeq(disk uuid.UUID) (seq *int64, ok bool) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(), "SELECT release_seq FROM disk_exports WHERE disk_id = $1", disk).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return seq, true
}

// detach removes every disk of v and returns the seq of the first list sent without them.
func (e *env) detach(node *fakeNode, v store.VM) uint64 {
	e.t.Helper()
	if _, _, err := e.s.SetVMDisks(context.Background(), v.OrgID, v.ID, nil, true); err != nil {
		e.t.Fatal(err)
	}
	e.f.NotifyOrg(v.OrgID)
	m := node.desired()
	if len(m.Desired) != 1 || len(m.Desired[0].Spec.Attachments) != 0 {
		e.t.Fatalf("list after detach: %+v", m.Desired)
	}
	return m.Seq
}

var barriers atomic.Int32

// barrier returns once the hub has handled every message the node sent before it.
func (e *env) barrier(node *fakeNode, v store.VM) {
	e.t.Helper()
	ip := fmt.Sprintf("10.0.%d.1", barriers.Add(1))
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning, IP: ip}})
	waitFor(e.t, func() bool {
		got, err := e.s.VM(context.Background(), v.OrgID, v.ID)
		return err == nil && got.IP == ip
	})
}

func TestDesiredCarriesAttachments(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	other, err := e.s.CreateOrg(ctx, "B", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	n, cred := e.node(e.org.ID, "n")
	b, bcred := e.node(other.ID, "b")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.vm(other.ID, b.ID, "foreign", &e.owner, nil)
	d := e.readyDisk(v, "data")

	m := e.connect(cred).desired()
	atts := m.Desired[0].Spec.Attachments
	if len(atts) != 1 {
		t.Fatalf("attachments = %+v", atts)
	}
	a := atts[0]
	if a.DiskID != d.ID.String() || a.URL != "nbd://10.198.0.1:10810/"+d.ID.String() || len(a.PSK) != 64 || a.MountPath != "/data" || a.SizeGB != 1 {
		t.Errorf("attachment = %+v", a)
	}
	for _, dv := range e.connect(bcred).desired().Desired {
		if len(dv.Spec.Attachments) != 0 {
			t.Errorf("foreign node got attachments %+v", dv.Spec.Attachments)
		}
	}
}

func TestReleaseSeqMarkedOnlyForListsWithoutDisk(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	d := e.readyDisk(v, "data")

	node := e.connect(cred)
	if len(node.desired().Desired[0].Spec.Attachments) != 1 {
		t.Fatal("attachment missing")
	}
	if seq, ok := e.releaseSeq(d.ID); !ok || seq != nil {
		t.Fatalf("release_seq before detach = %v %v", seq, ok)
	}
	s := e.detach(node, v)
	waitFor(t, func() bool { seq, _ := e.releaseSeq(d.ID); return seq != nil })
	if seq, _ := e.releaseSeq(d.ID); uint64(*seq) != s {
		t.Fatalf("release_seq = %d, want %d", *seq, s)
	}
	// Later lists keep the first seq.
	e.f.NotifyOrg(e.org.ID)
	node.desired()
	e.barrier(node, v)
	if seq, _ := e.releaseSeq(d.ID); uint64(*seq) != s {
		t.Fatalf("release_seq moved to %d", *seq)
	}
}

func TestStaleDisksMessageDoesNotRelease(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	d := e.readyDisk(v, "data")

	node := e.connect(cred)
	node.desired()
	s := e.detach(node, v)
	waitFor(t, func() bool { seq, _ := e.releaseSeq(d.ID); return seq != nil })

	node.send(proto.Message{Type: "disks", DisksInUse: []string{}, AppliedSeq: s - 1})
	node.send(proto.Message{Type: "disks", DisksInUse: []string{d.ID.String(), "not-a-uuid"}, AppliedSeq: s})
	e.barrier(node, v)
	if _, ok := e.releaseSeq(d.ID); !ok {
		t.Fatal("export released by a stale or in-use disks message")
	}
	node.send(proto.Message{Type: "disks", DisksInUse: []string{"not-a-uuid"}, AppliedSeq: s})
	waitFor(t, func() bool { _, ok := e.releaseSeq(d.ID); return !ok })
}

func TestStaleRegisterDoesNotRelease(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	d := e.readyDisk(v, "data")

	node := e.connect(cred)
	node.desired()
	s := e.detach(node, v)
	waitFor(t, func() bool { seq, _ := e.releaseSeq(d.ID); return seq != nil })
	node.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(n.ID) })

	node = e.register(cred, proto.Register{AppliedSeq: s - 1})
	node.desired()
	if _, ok := e.releaseSeq(d.ID); !ok {
		t.Fatal("export released by a register from before the detach")
	}
	node.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(n.ID) })
	e.register(cred, proto.Register{AppliedSeq: s}).desired()
	if _, ok := e.releaseSeq(d.ID); ok {
		t.Fatal("register after the detach did not release")
	}
}

func TestSeqMonotonicAcrossConnections(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	node := e.connect(cred)
	first := node.desired().Seq
	e.f.NotifyOrg(e.org.ID)
	last := node.desired().Seq
	if last <= first {
		t.Fatalf("seq %d after %d", last, first)
	}
	node.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(n.ID) })
	if got := e.connect(cred).desired().Seq; got <= last {
		t.Fatalf("seq after reconnect %d, last before %d", got, last)
	}
}

// A clock step back between hub runs must not let a new list's seq fall below a recorded release_seq.
func TestSeqStartsAboveRecordedReleaseSeqs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	d := e.readyDisk(v, "data")
	future := time.Now().Add(time.Hour).UnixNano()
	if _, err := e.pool.Exec(ctx, "UPDATE disk_exports SET releasing = true, release_seq = $2 WHERE disk_id = $1", d.ID, future); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.New(ctx, e.s, e.z, e.h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	e.f = f
	e.srv = httptest.NewServer(http.HandlerFunc(f.ServeConnect))
	t.Cleanup(e.srv.Close)
	if got := e.connect(cred).desired().Seq; got <= uint64(future) {
		t.Fatalf("seq %d not above release_seq %d", got, future)
	}
}

func TestDiskErrorsFromStatus(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	other, err := e.s.CreateOrg(ctx, "B", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	n, cred := e.node(e.org.ID, "n")
	b, _ := e.node(other.ID, "b")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	foreign := e.vm(other.ID, b.ID, "foreign", &e.owner, nil)
	id := uuid.NewString()

	node := e.connect(cred)
	node.desired()
	for _, vm := range []store.VM{foreign, v} {
		node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: vm.ID.String(), State: proto.StateRunning,
			Disks: []proto.Disk{{Name: "root"}, {Name: id, Error: "NBD connection lost"}}}})
	}
	waitFor(t, func() bool { return e.f.DiskErrors(v.ID)[id] == "NBD connection lost" })
	if got := e.f.DiskErrors(v.ID); len(got) != 1 {
		t.Errorf("disk errors = %v", got)
	}
	if got := e.f.DiskErrors(foreign.ID); len(got) != 0 {
		t.Errorf("foreign node set disk errors %v", got)
	}
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning, Disks: []proto.Disk{{Name: id}}}})
	waitFor(t, func() bool { return len(e.f.DiskErrors(v.ID)) == 0 })
}

func TestDiskErrorsDroppedWithVMOrNode(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	gone := e.vm(e.org.ID, n.ID, "gone", &e.owner, nil)
	kept := e.vm(e.org.ID, n.ID, "kept", &e.owner, nil)
	node := e.connect(cred)
	node.desired()
	for _, vm := range []store.VM{gone, kept} {
		node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: vm.ID.String(), State: proto.StateRunning,
			Disks: []proto.Disk{{Name: "d", Error: "NBD connection lost"}}}})
	}
	waitFor(t, func() bool { return len(e.f.DiskErrors(gone.ID)) == 1 && len(e.f.DiskErrors(kept.ID)) == 1 })
	if err := e.s.DeleteVM(ctx, e.org.ID, gone.ID, nil, store.DeleteReasonUser); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	node.desired()
	if len(e.f.DiskErrors(gone.ID)) != 0 || len(e.f.DiskErrors(kept.ID)) != 1 {
		t.Fatalf("after delete: gone %v kept %v", e.f.DiskErrors(gone.ID), e.f.DiskErrors(kept.ID))
	}
	e.f.Disconnect(n.ID)
	if got := e.f.DiskErrors(kept.ID); len(got) != 0 {
		t.Fatalf("after disconnect: %v", got)
	}
}

// reportSnapshot reports snap as taken by the node; without attached disks it becomes ready at once.
func (e *env) reportSnapshot(v store.VM, snap uuid.UUID, errMsg string) {
	e.t.Helper()
	if _, err := e.s.ApplySnapshotStatus(context.Background(), v.NodeID, proto.VMStatus{ID: v.ID.String(),
		Snapshots: []proto.SnapshotInfo{{ID: snap.String(), Error: errMsg}}}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) snapshot(v store.VM, name string) store.Snapshot {
	e.t.Helper()
	sn, err := e.s.CreateSnapshot(context.Background(), v.OrgID, v.ID, name, false, e.owner)
	if err != nil {
		e.t.Fatal(err)
	}
	return sn
}

func desiredVM(t *testing.T, m proto.Message, id uuid.UUID) *proto.DesiredVM {
	t.Helper()
	for _, d := range m.Desired {
		if d.Spec.ID == id.String() {
			return &d
		}
	}
	return nil
}

func TestDesiredCarriesSnapshots(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	creating := e.snapshot(v, "creating")
	ready := e.snapshot(v, "ready")
	e.reportSnapshot(v, ready.ID, "")
	failed := e.snapshot(v, "failed")
	e.reportSnapshot(v, failed.ID, "disk full")

	got := e.connect(cred).desired().Desired[0].Snapshots
	want := []proto.SnapshotRef{{ID: creating.ID.String()}, {ID: ready.ID.String(), Complete: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("snapshots = %+v, want %+v", got, want)
	}
}

func TestDesiredRestoreReady(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	sn := e.snapshot(v, "s")
	e.reportSnapshot(v, sn.ID, "")
	if _, err := e.s.RequestRestore(ctx, e.org.ID, v.ID, sn.ID, false, true); err != nil {
		t.Fatal(err)
	}
	node := e.connect(cred)
	for _, c := range []struct {
		state string
		ready bool
	}{{"node", false}, {"storage", false}, {"done", true}} {
		if _, err := e.pool.Exec(ctx, "UPDATE vms SET restore_state = $2 WHERE id = $1", v.ID, c.state); err != nil {
			t.Fatal(err)
		}
		e.f.NotifyOrg(e.org.ID)
		want := proto.Restore{SnapshotID: sn.ID.String(), Seq: 1, Ready: c.ready}
		// A list computed before the update may still be queued; the kick guarantees a newer one follows.
		deadline := time.Now().Add(5 * time.Second)
		for {
			r := node.desired().Desired[0].Restore
			if r != nil && *r == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: restore = %+v", c.state, r)
			}
		}
	}
}

func TestDesiredSkipsForkUntilDisksReady(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.readyDisk(v, "data")
	sn := e.snapshot(v, "s")
	e.reportSnapshot(v, sn.ID, "")
	sn, err := e.s.Snapshot(ctx, e.org.ID, v.ID, sn.ID)
	if err != nil || len(sn.Disks) != 1 {
		t.Fatalf("snapshot: %+v %v", sn, err)
	}
	if _, err := e.s.ApplyStorageFiles(ctx, []proto.StorageFile{{Name: proto.StorageFileName(sn.Disks[0].ID.String()), SizeBytes: 1 << 30}}); err != nil {
		t.Fatal(err)
	}
	fork, err := e.s.ForkVM(ctx, e.org.ID, sn, "fork", false, e.owner)
	if err != nil {
		t.Fatal(err)
	}

	node := e.connect(cred)
	if m := node.desired(); desiredVM(t, m, fork.ID) != nil || desiredVM(t, m, v.ID) == nil {
		t.Fatalf("fork listed before its disk is ready: %v", ids(m))
	}
	disks, err := e.s.VMDisks(ctx, fork.ID)
	if err != nil || len(disks) != 1 {
		t.Fatalf("fork disks: %+v %v", disks, err)
	}
	if _, err := e.s.ApplyStorageFiles(ctx, []proto.StorageFile{{Name: proto.StorageFileName(disks[0].DiskID.String()), SizeBytes: 1 << 30}}); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	d := desiredVM(t, node.desired(), fork.ID)
	if d == nil || d.Fork == nil || *d.Fork != (proto.Fork{SourceVMID: v.ID.String(), SnapshotID: sn.ID.String()}) {
		t.Fatalf("fork entry = %+v", d)
	}
}

func TestStatusSnapshotNotifies(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	sn := e.snapshot(v, "s")
	node := e.connect(cred)
	if refs := node.desired().Desired[0].Snapshots; len(refs) != 1 || refs[0].Complete {
		t.Fatalf("snapshots = %+v", refs)
	}
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning,
		Snapshots: []proto.SnapshotInfo{{ID: sn.ID.String(), SizeBytes: 1}}}})
	if refs := node.desired().Desired[0].Snapshots; len(refs) != 1 || !refs[0].Complete {
		t.Fatalf("snapshots after status = %+v", refs)
	}
}

func TestSweepSnapshotsFailsStaleAndNotifies(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.readyDisk(v, "data")
	sn := e.snapshot(v, "s")
	e.reportSnapshot(v, sn.ID, "")
	node := e.connect(cred)
	if refs := node.desired().Desired[0].Snapshots; len(refs) != 1 {
		t.Fatalf("snapshots = %+v", refs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.f.SweepSnapshots(ctx, 10*time.Millisecond, 0)
	if refs := node.desired().Desired[0].Snapshots; len(refs) != 0 {
		t.Fatalf("snapshots after timeout = %+v", refs)
	}
	if got, _ := e.s.Snapshot(context.Background(), e.org.ID, v.ID, sn.ID); got.State != "error" {
		t.Fatalf("state %q", got.State)
	}
}

func TestDisconnectFailsSnapshotWaitingForCopies(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.readyDisk(v, "data")
	sn := e.snapshot(v, "s")
	node := e.connect(cred)
	node.desired()
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning,
		Snapshots: []proto.SnapshotInfo{{ID: sn.ID.String(), SizeBytes: 1}}}})
	node.desired()
	node.sess.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := e.s.Snapshot(context.Background(), e.org.ID, v.ID, sn.ID)
		if got.State == "error" && len(got.Disks) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot after disconnect: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) privateVar(org, user uuid.UUID, name, value string, hosts ...string) store.EnvVar {
	e.t.Helper()
	ctx := context.Background()
	v, err := e.s.CreateEnvVar(ctx, store.EnvVar{OrgID: org, Kind: store.EnvSecret, Scope: store.EnvScopeUser, OwnerID: &user, Name: name, Hosts: hosts, CreatedBy: e.owner, UpdatedBy: e.owner}, value)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		e.t.Fatal(err)
	}
	return v
}

const ghValue = "ghp_0123456789abcdef"

func TestDesiredEgressPlaceholder(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")

	d := e.connect(cred).desired().Desired[0]
	if ph := d.Boot.Env["GH_TOKEN"]; !strings.HasPrefix(ph, "obsph_") {
		t.Fatalf("env GH_TOKEN = %q", ph)
	}
	if d.Egress == nil || len(d.Egress.Secrets) != 1 || d.Egress.Secrets[0].Value != ghValue || d.Egress.Secrets[0].Placeholder != d.Boot.Env["GH_TOKEN"] {
		t.Fatalf("egress = %+v", d.Egress)
	}
	if len(d.Egress.CAs) != 1 || !strings.Contains(d.Egress.CAs[0].KeyPEM, "PRIVATE KEY") {
		t.Fatalf("cas = %d", len(d.Egress.CAs))
	}
	if ca := d.Egress.CAs[0]; len(d.Boot.EgressCAs) != 1 || d.Boot.EgressCAs[ca.ID] != ca.CertPEM {
		t.Fatalf("boot cas = %v", d.Boot.EgressCAs)
	}
	b, err := json.Marshal(d.Boot)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, ghValue) || strings.Contains(s, "PRIVATE KEY") {
		t.Fatalf("boot leaks: %s", s)
	}
	if s := fmt.Sprintf("%v %+v %#v", d, d, d); strings.Contains(s, ghValue) || strings.Contains(s, "PRIVATE KEY") {
		t.Fatalf("formatted desired leaks: %s", s)
	}
}

func TestDesiredNoEgressWithoutBound(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.privateVar(e.org.ID, e.owner, "PLAIN", ghValue)

	d := e.connect(cred).desired().Desired[0]
	if d.Egress != nil || len(d.Boot.EgressCAs) != 0 || d.Boot.Env["PLAIN"] != ghValue {
		t.Fatalf("egress = %+v, boot cas = %v, env = %v", d.Egress, d.Boot.EgressCAs, d.Boot.Env)
	}
}

func TestDesiredTeamVMNoPrivateBound(t *testing.T) {
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
	e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")
	e.vm(e.org.ID, n.ID, "team-vm", nil, &team.ID)

	d := e.connect(cred).desired().Desired[0]
	if _, ok := d.Boot.Env["GH_TOKEN"]; ok || d.Egress != nil {
		t.Fatalf("team VM env = %v, egress = %+v", d.Boot.Env, d.Egress)
	}
}

func TestDesiredOtherOrgNoValues(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	nA, credA := e.node(e.org.ID, "a")
	e.vm(e.org.ID, nA.ID, "vm-a", &e.owner, nil)
	e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")
	orgB, err := e.s.CreateOrg(ctx, "B", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	nB, credB := e.node(orgB.ID, "b")
	e.vm(orgB.ID, nB.ID, "vm-b", &e.owner, nil)

	placeholder := e.connect(credA).desired().Desired[0].Boot.Env["GH_TOKEN"]
	if placeholder == "" {
		t.Fatal("no placeholder for org A")
	}
	b, err := json.Marshal(e.connect(credB).desired())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{ghValue, placeholder, "PRIVATE KEY"} {
		if strings.Contains(string(b), s) {
			t.Errorf("org B desired contains %q", s)
		}
	}
}

func TestValueChangePushesSameGeneration(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	v := e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")

	node := e.connect(cred)
	first := node.desired().Desired[0]
	newValue := "ghp_fedcba9876543210"
	if err := e.s.UpdateEnvVar(ctx, v, v, &newValue); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	second := node.desired().Desired[0]
	if second.Egress == nil || second.Egress.Secrets[0].Value != newValue || second.Generation != first.Generation {
		t.Fatalf("generation %d → %d, egress %+v", first.Generation, second.Generation, second.Egress)
	}
	if second.Boot.Env["GH_TOKEN"] != first.Boot.Env["GH_TOKEN"] {
		t.Fatal("placeholder changed with the value")
	}
}

func TestSweepEgressCAsNotifies(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")

	node := e.connect(cred)
	if cas := node.desired().Desired[0].Egress.CAs; len(cas) != 1 {
		t.Fatalf("cas = %d", len(cas))
	}
	if _, err := e.pool.Exec(context.Background(), "UPDATE vm_egress_cas SET not_after = now() + interval '10 days' WHERE vm_id = $1", v.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.f.SweepEgressCAs(ctx, 10*time.Millisecond)
	if cas := node.desired().Desired[0].Egress.CAs; len(cas) != 2 {
		t.Fatalf("cas after sweep = %d", len(cas))
	}
}

func TestUnbindDropsEgressCAs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	vm := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	v := e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")

	node := e.connect(cred)
	node.desired()
	unbound := v
	unbound.Hosts = nil
	if err := e.s.UpdateEnvVar(ctx, v, unbound, nil); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	if d := node.desired().Desired[0]; d.Egress != nil || d.Boot.Env["GH_TOKEN"] != ghValue {
		t.Fatalf("egress = %+v, env = %v", d.Egress, d.Boot.Env)
	}
	var rows int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM vm_egress_cas WHERE vm_id = $1", vm.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("ca rows = %d, %v", rows, err)
	}
}

func hostRange(from, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("h%d.test", from+i)
	}
	return out
}

func TestDesiredEgressCAOverManySecrets(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.privateVar(e.org.ID, e.owner, "A_TOKEN", ghValue, hostRange(0, 6)...)
	e.privateVar(e.org.ID, e.owner, "B_TOKEN", ghValue, hostRange(6, 6)...)

	d := e.connect(cred).desired().Desired[0]
	if d.Egress == nil || len(d.Egress.CAs) != 1 {
		t.Fatalf("egress = %+v", d.Egress)
	}
	block, _ := pem.Decode([]byte(d.Egress.CAs[0].CertPEM))
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want := hostRange(0, 12)
	slices.Sort(want)
	if got := slices.Sorted(slices.Values(c.PermittedDNSDomains)); !slices.Equal(got, want) {
		t.Fatalf("permitted = %v", got)
	}
}

func TestDesiredEgressTooManyHostsKeepsVM(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	for i := range 11 {
		e.privateVar(e.org.ID, e.owner, fmt.Sprintf("T%d", i), ghValue, hostRange(i*10, 10)...)
	}
	d := desiredVM(t, e.connect(cred).desired(), v.ID)
	if d == nil || d.Egress != nil || len(d.Boot.EgressCAs) != 0 {
		t.Fatalf("desired = %+v", d)
	}
}

func TestDesiredEgressFailureKeepsOtherVMs(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	broken := e.vm(e.org.ID, n.ID, "broken", &e.owner, nil)
	ok := e.vm(e.org.ID, n.ID, "ok", &e.owner, nil)
	e.privateVar(e.org.ID, e.owner, "GH_TOKEN", ghValue, "api.github.com")

	node := e.connect(cred)
	node.desired()
	if _, err := e.pool.Exec(context.Background(), "UPDATE vm_egress_cas SET key_ciphertext = 'garbage' WHERE vm_id = $1", broken.ID); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	m := node.desired()
	if d := desiredVM(t, m, broken.ID); d == nil || d.Egress != nil {
		t.Fatalf("broken VM = %+v", d)
	}
	if d := desiredVM(t, m, ok.ID); d == nil || d.Egress == nil {
		t.Fatalf("ok VM = %+v", d)
	}
}

const (
	intToken   = "ghp_dummy0000000000000000"
	intRefresh = "rt-dummy-0000000000"
)

func (e *env) integration(kind integrations.Kind, source integrations.Source, scope store.EnvScope, owner *uuid.UUID, mode integrations.Mode, sec store.IntegrationSecret) store.Integration {
	e.t.Helper()
	ctx := context.Background()
	v, err := e.s.CreateIntegration(ctx, store.Integration{OrgID: e.org.ID, Kind: kind, Source: source, Scope: scope, OwnerID: owner, Mode: mode, CreatedBy: e.owner, UpdatedBy: e.owner}, sec)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestDesiredIntegrationProxy(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	e.integration(integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: intToken})

	d := e.connect(cred).desired().Desired[0]
	if len(d.Integrations) != 1 || d.Integrations[0].Kind != "github" || !strings.HasPrefix(d.Integrations[0].Token, "obsph_") || d.Integrations[0].ExpiresAt != nil {
		t.Fatalf("integrations = %#v", d.Integrations)
	}
	if d.Egress == nil || len(d.Egress.Secrets) != 1 {
		t.Fatalf("egress = %+v", d.Egress)
	}
	s := d.Egress.Secrets[0]
	if s.Name != "github integration" || s.Value != intToken || s.Placeholder != d.Integrations[0].Token ||
		!slices.Equal(s.Hosts, []string{"api.github.com", "github.com", "uploads.github.com"}) || !slices.Equal(s.Headers, []string{"Authorization"}) {
		t.Fatalf("secret = %+v", s)
	}
	if len(d.Egress.CAs) != 1 {
		t.Fatalf("cas = %d", len(d.Egress.CAs))
	}
	b, err := json.Marshal(d.Boot)
	if err != nil {
		t.Fatal(err)
	}
	if str := string(b); strings.Contains(str, intToken) || strings.Contains(str, s.Placeholder) {
		t.Fatalf("boot leaks: %s", str)
	}
}

func TestDesiredIntegrationVMMode(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	e.integration(integrations.Claude, integrations.SourceOAuth, store.EnvScopeUser, &e.owner, integrations.ModeVM, store.IntegrationSecret{Token: intToken, Refresh: intRefresh, AccessExpiresAt: &exp})

	d := e.connect(cred).desired().Desired[0]
	if len(d.Integrations) != 1 || d.Integrations[0].Token != intToken || d.Integrations[0].ExpiresAt == nil || !d.Integrations[0].ExpiresAt.Equal(exp) {
		t.Fatalf("integrations = %#v", d.Integrations)
	}
	if d.Egress != nil {
		t.Fatalf("egress = %+v", d.Egress)
	}
}

func TestDesiredTeamVMNoPrivateIntegration(t *testing.T) {
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
	e.integration(integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: intToken})
	e.vm(e.org.ID, n.ID, "team-vm", nil, &team.ID)

	d := e.connect(cred).desired().Desired[0]
	if len(d.Integrations) != 0 || d.Egress != nil {
		t.Fatalf("team VM integrations = %#v, egress = %+v", d.Integrations, d.Egress)
	}
}

func TestDesiredIntegrationsOtherOrg(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	nA, _ := e.node(e.org.ID, "a")
	e.vm(e.org.ID, nA.ID, "vm-a", &e.owner, nil)
	v := e.integration(integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: intToken})
	orgB, err := e.s.CreateOrg(ctx, "B", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	nB, credB := e.node(orgB.ID, "b")
	e.vm(orgB.ID, nB.ID, "vm-b", &e.owner, nil)
	m := e.connect(credB).desired()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := e.s.ResolveVMIntegrations(ctx, e.z.Enforcer(), e.org.ID, authz.UserSubject(e.owner))
	if err != nil || len(resolved) != 1 || resolved[0].ID != v.ID {
		t.Fatalf("resolve: %v %v", resolved, err)
	}
	if s := string(b); strings.Contains(s, intToken) || strings.Contains(s, resolved[0].Placeholder) {
		t.Fatalf("org B desired leaks: %s", s)
	}
}

func TestIntegrationChangePushes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	in := e.integration(integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: intToken})

	node := e.connect(cred)
	before := desiredVM(t, node.desired(), v.ID)
	if before.Integrations[0].Token == intToken {
		t.Fatal("proxy mode delivered the value")
	}
	if err := e.s.SetIntegrationMode(ctx, in, integrations.ModeVM, e.owner); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	after := desiredVM(t, node.desired(), v.ID)
	if after.Integrations[0].Token != intToken || after.Integrations[0].ExpiresAt != nil || after.Generation != before.Generation {
		t.Fatalf("after mode change: %#v gen %d vs %d", after.Integrations, after.Generation, before.Generation)
	}
	cur, err := e.s.Integration(ctx, e.org.ID, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteIntegration(ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	if d := desiredVM(t, node.desired(), v.ID); len(d.Integrations) != 0 {
		t.Fatalf("after delete: %#v", d.Integrations)
	}
}

func TestDesiredNeverRefreshToken(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	exp := time.Now().Add(time.Hour)
	for _, mode := range []integrations.Mode{integrations.ModeProxy, integrations.ModeVM} {
		in := e.integration(integrations.Claude, integrations.SourceOAuth, store.EnvScopeUser, &e.owner, mode, store.IntegrationSecret{Token: "sk-ant-oat01-dummyaccess0000", Refresh: intRefresh, AccessExpiresAt: &exp})
		b, err := json.Marshal(e.connect(cred).desired())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), intRefresh) {
			t.Fatalf("%s mode leaks refresh token", mode)
		}
		if err := e.s.DeleteIntegration(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
}

type fakeOAuth struct {
	mu    sync.Mutex
	calls int
	err   error
	exp   time.Time
}

func (*fakeOAuth) AuthorizeURL(string, string) string { return "" }
func (*fakeOAuth) Exchange(context.Context, string, string, string) (integrations.OAuthTokens, error) {
	return integrations.OAuthTokens{}, errors.New("unused")
}
func (f *fakeOAuth) Refresh(_ context.Context, rt string) (integrations.OAuthTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return integrations.OAuthTokens{}, f.err
	}
	return integrations.OAuthTokens{Access: "at-new-dummy-0000", Refresh: "rt-new-dummy-0000", ExpiresAt: f.exp}, nil
}

func (f *fakeOAuth) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// runRefresh starts the refresh loop and stops it, waiting for its exit, at test end.
func (e *env) runRefresh(oauth integrations.ClaudeOAuth, google integrations.GoogleOAuth) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.f.RefreshIntegrations(ctx, oauth, google, 10*time.Millisecond)
		close(done)
	}()
	e.t.Cleanup(func() { cancel(); <-done })
}

func TestRefreshIntegrationsPushes(t *testing.T) {
	for _, mode := range []integrations.Mode{integrations.ModeProxy, integrations.ModeVM} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnv(t)
			n, cred := e.node(e.org.ID, "n")
			v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
			exp := time.Now().Add(10 * time.Minute)
			e.integration(integrations.Claude, integrations.SourceOAuth, store.EnvScopeUser, &e.owner, mode, store.IntegrationSecret{Token: "sk-ant-oat01-dummyaccess0000", Refresh: intRefresh, AccessExpiresAt: &exp})
			node := e.connect(cred)
			before := desiredVM(t, node.desired(), v.ID)
			newExp := time.Now().Add(8 * time.Hour).Truncate(time.Second)
			e.runRefresh(&fakeOAuth{exp: newExp}, &fakeGoogle{})
			after := desiredVM(t, node.desired(), v.ID)
			if mode == integrations.ModeProxy {
				if after.Egress == nil || len(after.Egress.Secrets) != 1 || after.Egress.Secrets[0].Value != "at-new-dummy-0000" ||
					after.Egress.Secrets[0].Placeholder != before.Egress.Secrets[0].Placeholder || after.Integrations[0].Token != before.Integrations[0].Token {
					t.Fatalf("proxy after refresh: %+v", after.Egress)
				}
				return
			}
			if len(after.Integrations) != 1 || after.Integrations[0].Token != "at-new-dummy-0000" || after.Integrations[0].ExpiresAt == nil || !after.Integrations[0].ExpiresAt.Equal(newExp) {
				t.Fatalf("vm after refresh: %#v", after.Integrations)
			}
		})
	}
}

type fakeGoogle struct {
	mu   sync.Mutex
	adcs []string
	err  error
}

func (f *fakeGoogle) Refresh(_ context.Context, adc string) (integrations.OAuthTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adcs = append(f.adcs, adc)
	if f.err != nil {
		return integrations.OAuthTokens{}, f.err
	}
	return integrations.OAuthTokens{Access: "ya29.new-dummy-0000", Refresh: adc, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (*fakeGoogle) Email(context.Context, string) (string, error) { return "", errors.New("unused") }

func (f *fakeGoogle) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.adcs)
}

func TestRefreshGcloud(t *testing.T) {
	adc := integrations.ADC{ClientID: "cid", ClientSecret: "dummy-secret", RefreshToken: intRefresh}.Marshal()
	t.Run("due", func(t *testing.T) {
		e := newEnv(t)
		n, cred := e.node(e.org.ID, "n")
		v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
		exp := time.Now().Add(5 * time.Minute)
		e.integration(integrations.GCloud, integrations.SourceADC, store.EnvScopeUser, &e.owner, integrations.ModeProxy,
			store.IntegrationSecret{Token: "ya29.old-dummy-0000", Refresh: adc, AccessExpiresAt: &exp})
		node := e.connect(cred)
		before := desiredVM(t, node.desired(), v.ID)
		g := &fakeGoogle{}
		e.runRefresh(&fakeOAuth{}, g)
		after := desiredVM(t, node.desired(), v.ID)
		if after.Egress == nil || len(after.Egress.Secrets) != 1 || after.Egress.Secrets[0].Value != "ya29.new-dummy-0000" ||
			after.Egress.Secrets[0].Placeholder != before.Egress.Secrets[0].Placeholder {
			t.Fatalf("after refresh: %+v", after.Egress)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.adcs) != 1 || g.adcs[0] != adc {
			t.Fatalf("google refreshes: %d", len(g.adcs))
		}
	})
	// Claude refreshes 60 minutes ahead, gcloud only 10.
	t.Run("not due", func(t *testing.T) {
		e := newEnv(t)
		exp := time.Now().Add(15 * time.Minute)
		e.integration(integrations.GCloud, integrations.SourceADC, store.EnvScopeUser, &e.owner, integrations.ModeProxy,
			store.IntegrationSecret{Token: "ya29.old-dummy-0000", Refresh: adc, AccessExpiresAt: &exp})
		g := &fakeGoogle{}
		e.runRefresh(&fakeOAuth{}, g)
		time.Sleep(50 * time.Millisecond)
		if g.n() != 0 {
			t.Fatalf("refreshed %d times", g.n())
		}
	})
}

func TestIntegrationEntriesProject(t *testing.T) {
	res := []store.ResolvedIntegration{
		{Integration: store.Integration{ID: uuid.New(), Kind: integrations.GCloud, Mode: integrations.ModeProxy, QuotaProject: "bna-trader-tf-state"}, Placeholder: "obsph_g", Token: "ya29.x"},
		{Integration: store.Integration{ID: uuid.New(), Kind: integrations.GitHub, Mode: integrations.ModeProxy}, Placeholder: "obsph_h", Token: intToken},
	}
	_, tokens := fleet.IntegrationEntries(res)
	if len(tokens) != 2 || tokens[0].Project != "bna-trader-tf-state" || tokens[0].Token != "obsph_g" || tokens[1].Project != "" {
		t.Fatalf("tokens: %+v", tokens)
	}
}

func TestRefreshIntegrationsBackoff(t *testing.T) {
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	base := time.Now()
	var offset atomic.Int64
	prevClock := *fleet.Clock
	*fleet.Clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { *fleet.Clock = prevClock })

	e := newEnv(t)
	exp := base.Add(10 * time.Minute)
	in := e.integration(integrations.Claude, integrations.SourceOAuth, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: "sk-ant-oat01-dummyaccess0000", Refresh: intRefresh, AccessExpiresAt: &exp})
	oauth := &fakeOAuth{err: fmt.Errorf("%w: claude: HTTP 503", integrations.ErrUnreachable)}
	e.runRefresh(oauth, &fakeGoogle{})
	waitFor(t, func() bool { return oauth.n() == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := oauth.n(); n != 1 {
		t.Fatalf("called %d times within three ticks", n)
	}
	offset.Store(int64(59 * time.Second))
	time.Sleep(30 * time.Millisecond)
	if n := oauth.n(); n != 1 {
		t.Fatalf("called %d times before a minute passed", n)
	}
	offset.Store(int64(61 * time.Second))
	waitFor(t, func() bool { return oauth.n() == 2 })
	offset.Store(int64(61*time.Second + 119*time.Second))
	time.Sleep(30 * time.Millisecond)
	if n := oauth.n(); n != 2 {
		t.Fatalf("backoff did not double: %d calls", n)
	}
	logs := buf.String()
	if !strings.Contains(logs, "integration refresh failed") || !strings.Contains(logs, in.ID.String()) {
		t.Fatalf("no failure log: %s", logs)
	}
	for _, tok := range []string{"sk-ant-oat01-dummyaccess0000", intRefresh} {
		if strings.Contains(logs, tok) {
			t.Fatalf("token in log: %s", logs)
		}
	}
}

// gcloud refreshes only 10 minutes ahead, so its backoff stops at 2 minutes.
func TestRefreshGcloudBackoffCap(t *testing.T) {
	base := time.Now()
	var offset atomic.Int64
	prevClock := *fleet.Clock
	*fleet.Clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { *fleet.Clock = prevClock })

	e := newEnv(t)
	exp := base.Add(5 * time.Minute)
	adc := integrations.ADC{ClientID: "cid", ClientSecret: "dummy-secret", RefreshToken: intRefresh}.Marshal()
	e.integration(integrations.GCloud, integrations.SourceADC, store.EnvScopeUser, &e.owner, integrations.ModeProxy,
		store.IntegrationSecret{Token: "ya29.old-dummy-0000", Refresh: adc, AccessExpiresAt: &exp})
	g := &fakeGoogle{err: fmt.Errorf("%w: gcloud: HTTP 503", integrations.ErrUnreachable)}
	e.runRefresh(&fakeOAuth{}, g)
	waitFor(t, func() bool { return g.n() == 1 })
	offset.Store(int64(61 * time.Second))
	waitFor(t, func() bool { return g.n() == 2 })
	offset.Store(int64(182 * time.Second))
	waitFor(t, func() bool { return g.n() == 3 })
	offset.Store(int64(303 * time.Second))
	waitFor(t, func() bool { return g.n() == 4 })
}

func TestDesiredIntegrationsUnresolvedKeepsOtherVMs(t *testing.T) {
	old := *fleet.IntegrationRetry
	*fleet.IntegrationRetry = 50 * time.Millisecond
	t.Cleanup(func() { *fleet.IntegrationRetry = old })
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
	broken := e.vm(e.org.ID, n.ID, "broken", &e.owner, nil)
	ok := e.vm(e.org.ID, n.ID, "ok", nil, &team.ID)
	e.integration(integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil, integrations.ModeVM, store.IntegrationSecret{Token: intToken})
	bad := e.integration(integrations.Linear, integrations.SourcePAT, store.EnvScopeUser, &e.owner, integrations.ModeVM, store.IntegrationSecret{Token: intToken})
	if _, err := e.pool.Exec(ctx, "UPDATE integrations SET ciphertext = 'garbage' WHERE id = $1", bad.ID); err != nil {
		t.Fatal(err)
	}

	node := e.connect(cred)
	m := node.desired()
	if d := desiredVM(t, m, broken.ID); d == nil || !d.IntegrationsUnresolved || len(d.Integrations) != 0 {
		t.Fatalf("broken VM = %+v", d)
	}
	if d := desiredVM(t, m, ok.ID); d == nil || d.IntegrationsUnresolved || len(d.Integrations) != 1 {
		t.Fatalf("ok VM = %+v", d)
	}
	// The hub pushes again by itself until the integration resolves.
	if again := node.desired(); again.Seq <= m.Seq {
		t.Fatalf("seq %d after %d", again.Seq, m.Seq)
	}
	if _, err := e.pool.Exec(ctx, "DELETE FROM integrations WHERE id = $1", bad.ID); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	for range 5 {
		if d := desiredVM(t, node.desired(), broken.ID); !d.IntegrationsUnresolved {
			return
		}
	}
	t.Fatal("still unresolved after the integration was deleted")
}

func TestFleetStoresImage(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	node := e.register(cred, proto.Register{Image: &proto.ImageStatus{Version: "v1", State: proto.ImageReady}})
	image := func() *proto.ImageStatus {
		got, err := e.s.Node(context.Background(), e.org.ID, n.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Image
	}
	waitFor(t, func() bool { i := image(); return i != nil && i.State == proto.ImageReady })
	node.send(proto.Message{Type: "image", Image: &proto.ImageStatus{Version: "v1", State: proto.ImageFailed, Error: "boom"}})
	waitFor(t, func() bool { i := image(); return i != nil && i.State == proto.ImageFailed && i.Error == "boom" })
}

func TestDesiredVMImage(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	iss, err := registry.LoadIssuer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.f.PullImagesFrom(&url.URL{Scheme: "https", Host: "hub.test"}, iss)
	img, err := e.s.CreateImage(ctx, store.Image{OrgID: e.org.ID, Scope: store.EnvScopeOrg, Name: "tools", CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	digest, unknown := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	if err := e.s.AddImageVersion(ctx, store.ImageVersion{ImageID: img.ID, Digest: digest, Source: store.VersionFromPush, CreatedBy: &e.owner}); err != nil {
		t.Fatal(err)
	}
	n, cred := e.node(e.org.ID, "a")
	setImage := func(v store.VM, ref, d string) {
		if _, err := e.pool.Exec(ctx, "UPDATE vms SET image_ref = $2, image_digest = $3 WHERE id = $1", v.ID, ref, d); err != nil {
			t.Fatal(err)
		}
	}
	user := e.vm(e.org.ID, n.ID, "user", &e.owner, nil)
	setImage(user, "org/tools", digest)
	platform := e.vm(e.org.ID, n.ID, "platform", &e.owner, nil)
	setImage(platform, "observatory/standard", unknown)
	e.vm(e.org.ID, n.ID, "plain", &e.owner, nil)
	gone := e.vm(e.org.ID, n.ID, "gone", &e.owner, nil)
	setImage(gone, "org/gone", unknown)
	other, err := e.s.CreateImage(ctx, store.Image{OrgID: e.org.ID, Scope: store.EnvScopeOrg, Name: "other", CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddImageVersion(ctx, store.ImageVersion{ImageID: other.ID, Digest: digest, Source: store.VersionFromPush, CreatedBy: &e.owner}); err != nil {
		t.Fatal(err)
	}
	byID := e.vm(e.org.ID, n.ID, "byid", &e.owner, nil)
	setImage(byID, "org/other", digest)
	if _, err := e.pool.Exec(ctx, "UPDATE vms SET image_id = $2 WHERE id = $1", byID.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	staleID := e.vm(e.org.ID, n.ID, "staleid", &e.owner, nil)
	setImage(staleID, "org/tools", unknown)
	if _, err := e.pool.Exec(ctx, "UPDATE vms SET image_id = $2 WHERE id = $1", staleID.ID, img.ID); err != nil {
		t.Fatal(err)
	}
	prepared := e.vm(e.org.ID, n.ID, "prepared", &e.owner, nil)
	setImage(prepared, "org/tools", digest)
	if err := e.s.SetVMStatus(ctx, n.ID, proto.VMStatus{ID: prepared.ID.String(), State: proto.StateRunning, Image: "hub.test/i/x@" + digest}); err != nil {
		t.Fatal(err)
	}

	got := map[string]*proto.ImageRef{}
	for _, d := range e.connect(cred).desired().Desired {
		got[d.Spec.Name] = d.Spec.Image
	}
	if got["platform"] != nil || got["plain"] != nil || got["prepared"] != nil {
		t.Fatalf("image sent: platform %+v, plain %+v, prepared %+v", got["platform"], got["plain"], got["prepared"])
	}
	if g := got["gone"]; g == nil || !g.Gone || g.Repo != "" || g.PullToken != "" || g.Digest != unknown {
		t.Fatalf("gone image = %+v", g)
	}
	if b := got["byid"]; b == nil || b.Repo != "hub.test/i/"+other.ID.String() {
		t.Fatalf("image_id not preferred: %+v", b)
	}
	if g := got["staleid"]; g == nil || !g.Gone {
		t.Fatalf("missing version under image_id = %+v", g)
	}
	u := got["user"]
	if u == nil || u.Repo != "hub.test/i/"+img.ID.String() || u.Digest != digest {
		t.Fatalf("user image = %+v", u)
	}
	var c registry.Claims
	if _, _, err := jwt.NewParser().ParseUnverified(u.PullToken, &c); err != nil {
		t.Fatal(err)
	}
	if c.Subject != "node-org:"+e.org.ID.String() || len(c.Access) != 1 || c.Access[0].Name != "i/"+img.ID.String() || !slices.Equal(c.Access[0].Actions, []string{"pull"}) {
		t.Fatalf("claims = %+v", c)
	}
}

func TestDesiredVMImageIndex(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	iss, err := registry.LoadIssuer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.f.PullImagesFrom(&url.URL{Scheme: "https", Host: "hub.test"}, iss)
	img, err := e.s.CreateImage(ctx, store.Image{OrgID: e.org.ID, Scope: store.EnvScopeOrg, Name: "tools", CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	index, child := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("c", 64)
	if err := e.s.AddImageVersion(ctx, store.ImageVersion{ImageID: img.ID, Digest: index, PlatformDigest: child, Source: store.VersionFromPush, CreatedBy: &e.owner}); err != nil {
		t.Fatal(err)
	}
	n, cred := e.node(e.org.ID, "a")
	for _, name := range []string{"fresh", "prepared"} {
		v := e.vm(e.org.ID, n.ID, name, &e.owner, nil)
		if _, err := e.pool.Exec(ctx, "UPDATE vms SET image_ref = 'org/tools', image_digest = $2, image_id = $3 WHERE id = $1", v.ID, index, img.ID); err != nil {
			t.Fatal(err)
		}
		if name == "prepared" {
			if err := e.s.SetVMStatus(ctx, n.ID, proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning, Image: "hub.test/i/x@" + child}); err != nil {
				t.Fatal(err)
			}
		}
	}
	node := e.connect(cred)
	got := map[string]*proto.ImageRef{}
	for _, d := range node.desired().Desired {
		got[d.Spec.Name] = d.Spec.Image
	}
	if got["prepared"] != nil {
		t.Fatalf("image sent to a VM prepared from the amd64 child: %+v", got["prepared"])
	}
	if f := got["fresh"]; f == nil || f.Digest != child || f.Repo != "hub.test/i/"+img.ID.String() {
		t.Fatalf("fresh image = %+v, want the amd64 child", f)
	}
	if err := e.s.DeleteImageVersion(ctx, img.ID, index); err != nil {
		t.Fatal(err)
	}
	e.f.NotifyOrg(e.org.ID)
	got = map[string]*proto.ImageRef{}
	for _, d := range node.desired().Desired {
		got[d.Spec.Name] = d.Spec.Image
	}
	if got["prepared"] != nil || got["fresh"] == nil || !got["fresh"].Gone {
		t.Fatalf("after deleting the version: prepared %+v, fresh %+v", got["prepared"], got["fresh"])
	}
}

func TestImagePullRetried(t *testing.T) {
	ctx := context.Background()
	old := *fleet.ImageRetryMin
	*fleet.ImageRetryMin = 50 * time.Millisecond
	t.Cleanup(func() { *fleet.ImageRetryMin = old })
	e := newEnv(t)
	iss, err := registry.LoadIssuer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.f.PullImagesFrom(&url.URL{Scheme: "https", Host: "hub.test"}, iss)
	img, err := e.s.CreateImage(ctx, store.Image{OrgID: e.org.ID, Scope: store.EnvScopeOrg, Name: "tools", CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := e.s.AddImageVersion(ctx, store.ImageVersion{ImageID: img.ID, Digest: digest, Source: store.VersionFromPush, CreatedBy: &e.owner}); err != nil {
		t.Fatal(err)
	}
	n, cred := e.node(e.org.ID, "a")
	v := e.vm(e.org.ID, n.ID, "user", &e.owner, nil)
	if _, err := e.pool.Exec(ctx, "UPDATE vms SET image_ref = 'org/tools', image_digest = $2 WHERE id = $1", v.ID, digest); err != nil {
		t.Fatal(err)
	}
	node := e.connect(cred)
	token := func(m proto.Message) string {
		t.Helper()
		if len(m.Desired) != 1 || m.Desired[0].Spec.Image == nil {
			t.Fatalf("desired = %+v", m.Desired)
		}
		return m.Desired[0].Spec.Image.PullToken
	}
	first := token(node.desired())
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateError, Error: "image pull failed: UNAUTHORIZED"}})
	// Nothing changed on the hub: only the retry pushes again, with a new token.
	if second := token(node.desired()); second == "" || second == first {
		t.Fatalf("retry token %q after %q", second, first)
	}
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning, Image: "hub.test/i/x@" + digest}})
	// Retries go on until a list finds the image prepared; then the pushes stop.
	for m := node.desired(); m.Desired[0].Spec.Image != nil; m = node.desired() {
	}
	select {
	case m := <-node.msgs:
		t.Fatalf("push after the image was prepared: %+v", m)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestDesiredBuilderVM(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.envVar("OWNER_SECRET", "owner")
	e.sshKey(e.owner)
	digest := "sha256:" + strings.Repeat("c", 64)
	n, cred := e.node(e.org.ID, "a")
	builder, err := e.s.CreateVM(ctx, store.VM{OrgID: e.org.ID, NodeID: n.ID, UserID: &e.owner, Name: "build-x", CPUs: 2, MemoryMB: 4096, DiskGB: 40, CreatedBy: e.owner,
		Kind: store.VMKindBuilder, ImageRef: "ghcr.io/asterism-dev/observatory-builder@" + digest, ImageDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	e.f.BootBuildersWith(func(_ context.Context, v store.VM) (map[string]string, []proto.IntegrationToken, error) {
		if v.ID != builder.ID {
			t.Errorf("hook called for %s", v.Name)
		}
		return map[string]string{"OBSERVATORY_BUILD_ID": "b"}, []proto.IntegrationToken{{Kind: "github", Token: "real"}}, nil
	})
	got := e.connect(cred).desired().Desired
	if len(got) != 1 {
		t.Fatalf("desired: %+v", got)
	}
	d := got[0]
	if img := d.Spec.Image; img == nil || img.Repo != "ghcr.io/asterism-dev/observatory-builder" || img.Digest != digest || img.PullToken != "" || img.Gone {
		t.Fatalf("builder image = %+v", d.Spec.Image)
	}
	if len(d.Boot.Env) != 1 || d.Boot.Env["OBSERVATORY_BUILD_ID"] != "b" || len(d.Boot.SSHPublicKeys) != 0 || d.Egress != nil {
		t.Fatalf("builder boot = %+v, egress %+v", d.Boot, d.Egress)
	}
	if len(d.Integrations) != 1 || d.Integrations[0].Token != "real" {
		t.Fatalf("builder integrations = %+v", d.Integrations)
	}
	if !d.Spec.NoTemplate {
		t.Fatal("builder spec allows a fast-boot template")
	}
}

func TestDesiredImageNodeURL(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		hub       string
		plainHTTP bool
	}{{"https://hub.test", false}, {"http://100.64.0.1:8090", true}} {
		e := newEnv(t)
		iss, err := registry.LoadIssuer(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		hub, _ := url.Parse(tc.hub)
		e.f.PullImagesFrom(hub, iss)
		img, err := e.s.CreateImage(ctx, store.Image{OrgID: e.org.ID, Scope: store.EnvScopeOrg, Name: "tools", CreatedBy: e.owner})
		if err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + strings.Repeat("a", 64)
		if err := e.s.AddImageVersion(ctx, store.ImageVersion{ImageID: img.ID, Digest: digest, Source: store.VersionFromPush, CreatedBy: &e.owner}); err != nil {
			t.Fatal(err)
		}
		n, cred := e.node(e.org.ID, "a")
		e.vm(e.org.ID, n.ID, "user", &e.owner, nil)
		if _, err := e.pool.Exec(ctx, "UPDATE vms SET image_ref = 'org/tools', image_digest = $1, image_id = $2", digest, img.ID); err != nil {
			t.Fatal(err)
		}
		got := e.connect(cred).desired().Desired[0].Spec.Image
		if want := hub.Host + "/i/" + img.ID.String(); got.Repo != want || got.PlainHTTP != tc.plainHTTP {
			t.Errorf("%s: repo %q plainHTTP %v, want %q %v", tc.hub, got.Repo, got.PlainHTTP, want, tc.plainHTTP)
		}
	}
}

func TestRefreshInactiveAccount(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	far := time.Now().Add(8 * time.Hour)
	in := e.integration(integrations.Claude, integrations.SourceOAuth, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: "sk-ant-oat01-dummyaccess0000", Refresh: intRefresh, AccessExpiresAt: &far})
	soon := time.Now().Add(10 * time.Minute)
	acc, err := e.s.AddIntegrationAccount(context.Background(), in, store.IntegrationAccount{Account: "two@example.com", Source: integrations.SourceOAuth, CreatedBy: e.owner},
		store.IntegrationSecret{Token: "sk-ant-oat01-dummyaccess0001", Refresh: intRefresh, AccessExpiresAt: &soon})
	if err != nil {
		t.Fatal(err)
	}
	node := e.connect(cred)
	node.desired()
	newExp := time.Now().Add(8 * time.Hour).Truncate(time.Second)
	oauth := &fakeOAuth{exp: newExp}
	e.runRefresh(oauth, &fakeGoogle{})
	node.desired()
	if n := oauth.n(); n != 1 {
		t.Fatalf("refresh called %d times, want 1", n)
	}
	accs, err := e.s.IntegrationAccounts(context.Background(), e.org.ID, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accs {
		if a.ID == acc.ID && (a.Active || a.AccessExpiresAt == nil || !a.AccessExpiresAt.Equal(newExp)) {
			t.Fatalf("inactive account after refresh: %+v", a)
		}
	}
}

const (
	claudeTok1 = "sk-ant-oat01-dummyaccess0001"
	claudeTok2 = "sk-ant-oat01-dummyaccess0002"
)

// claudeAccounts creates the owner's two-account Claude integration in proxy mode; the first account is active.
func (e *env) claudeAccounts() (store.Integration, store.IntegrationAccount, store.IntegrationAccount) {
	e.t.Helper()
	ctx := context.Background()
	cl := e.integration(integrations.Claude, integrations.SourceSetupToken, store.EnvScopeUser, &e.owner, integrations.ModeProxy, store.IntegrationSecret{Token: claudeTok1})
	if _, err := e.s.AddIntegrationAccount(ctx, cl, store.IntegrationAccount{Account: "two@x.io", Source: integrations.SourceSetupToken, CreatedBy: e.owner}, store.IntegrationSecret{Token: claudeTok2}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.s.SetIntegrationAutoSwitch(ctx, cl, true, e.owner); err != nil {
		e.t.Fatal(err)
	}
	cl.AutoSwitch = true
	list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
	if err != nil || len(list) != 2 || !list[0].Active {
		e.t.Fatalf("accounts = %+v %v", list, err)
	}
	return cl, list[0], list[1]
}

func TestAutoSwitchOffSendsNoAlternates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	cl, first, _ := e.claudeAccounts()
	if err := e.s.SetIntegrationAutoSwitch(ctx, cl, false, e.owner); err != nil {
		t.Fatal(err)
	}
	node := e.connect(cred)
	s := desiredVM(t, node.desired(), v.ID).Egress.Secrets[0]
	if s.Account != first.ID.String() || s.Value != claudeTok1 || len(s.Alternates) != 0 {
		t.Fatalf("account %s, %d alternates", s.Account, len(s.Alternates))
	}
}

func TestUsageReportSwitchesAndPushes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	cl, first, second := e.claudeAccounts()

	node := e.connect(cred)
	s := desiredVM(t, node.desired(), v.ID).Egress.Secrets[0]
	if s.ID != "integration:"+cl.ID.String() || s.Account != first.ID.String() || s.Value != claudeTok1 ||
		len(s.Alternates) != 1 || s.Alternates[0].Account != second.ID.String() || s.Alternates[0].Value != claudeTok2 {
		t.Fatalf("before: %s %s %d", s.ID, s.Account, len(s.Alternates))
	}
	placeholder := s.Placeholder
	until := time.Now().Add(time.Hour).UTC()
	node.send(proto.Message{Type: "usage", Usage: []proto.UsageObservation{{Integration: cl.ID.String(), Account: first.ID.String(), LimitedUntil: &until, ObservedAt: time.Now()}}})
	s = desiredVM(t, node.desired(), v.ID).Egress.Secrets[0]
	// The guest's env holds the placeholder, so a switch must not change it.
	if s.Account != second.ID.String() || s.Value != claudeTok2 || placeholder == "" || s.Placeholder != placeholder {
		t.Fatalf("after: account %s, placeholder changed %v", s.Account, s.Placeholder != placeholder)
	}
	list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
	if err != nil || list[0].Active || !list[1].Active || list[0].LimitedUntil == nil || list[0].Usage.Source != "proxy" {
		t.Fatalf("accounts = %+v %v", list, err)
	}
}

func TestUsageReportForeignAccountIgnored(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	nA, _ := e.node(e.org.ID, "a")
	e.vm(e.org.ID, nA.ID, "vm", &e.owner, nil)
	nB, credB := e.node(e.org.ID, "b")
	team, err := e.s.CreateTeam(ctx, e.org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	e.vm(e.org.ID, nB.ID, "team-vm", nil, &team.ID)
	cl, first, _ := e.claudeAccounts()
	e.connect(credB).desired()

	until := time.Now().Add(time.Hour)
	pct := 50.0
	e.f.NoteUsage(ctx, nB, []proto.UsageObservation{
		{Integration: cl.ID.String(), Account: first.ID.String(), FiveHourPct: &pct, LimitedUntil: &until, ObservedAt: time.Now()},
		{Integration: uuid.NewString(), Account: first.ID.String(), LimitedUntil: &until, ObservedAt: time.Now()},
	})
	list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
	if err != nil || !list[0].Active || list[0].LimitedUntil != nil || list[0].Usage.FiveHourPct != nil {
		t.Fatalf("accounts = %+v %v", list, err)
	}
}

type fakeUsage struct {
	mu    sync.Mutex
	calls map[string]int
	fn    func(token string) (integrations.Usage, error)
}

func (f *fakeUsage) Usage(_ context.Context, token string) (integrations.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[token]++
	return f.fn(token)
}

func (f *fakeUsage) n(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[token]
}

func (e *env) runPoll(u integrations.ClaudeUsage) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.f.PollUsage(ctx, u, 5*time.Millisecond)
		close(done)
	}()
	e.t.Cleanup(func() { cancel(); <-done })
}

func TestPollUsageIntervals(t *testing.T) {
	e := newEnv(t)
	e.claudeAccounts()
	u := &fakeUsage{fn: func(string) (integrations.Usage, error) { return integrations.Usage{}, nil }}
	e.runPoll(u)
	waitFor(t, func() bool { return u.n(claudeTok2) == 2 })
	if n := u.n(claudeTok1); n < 16 {
		t.Fatalf("active polled %d times by tick 15", n)
	}
	time.Sleep(20 * time.Millisecond)
	if n := u.n(claudeTok2); n != 2 {
		t.Fatalf("inactive polled %d times, want ticks 0 and 15 only", n)
	}
}

func TestPollUsageLimitSwitches(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	cl, first, second := e.claudeAccounts()
	node := e.connect(cred)
	node.desired()

	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	full, low := 100.0, 10.0
	u := &fakeUsage{fn: func(tok string) (integrations.Usage, error) {
		if tok == claudeTok1 {
			return integrations.Usage{FiveHourPct: &full, FiveHourResetsAt: &reset, WeekPct: &low}, nil
		}
		return integrations.Usage{FiveHourPct: &low}, nil
	}}
	e.runPoll(u)
	waitFor(t, func() bool {
		list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
		return err == nil && !list[0].Active && list[1].Active
	})
	list, _ := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
	if list[0].LimitedUntil == nil || !list[0].LimitedUntil.Equal(reset) || list[0].Usage.Source != "poll" || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("accounts = %+v", list)
	}
	if s := desiredVM(t, node.desired(), v.ID).Egress.Secrets[0]; s.Account != second.ID.String() {
		t.Fatalf("pushed account %s", s.Account)
	}
}

func TestPollUsageSetupTokenRejectedStops(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	cl, _, _ := e.claudeAccounts()
	u := &fakeUsage{fn: func(string) (integrations.Usage, error) {
		return integrations.Usage{}, fmt.Errorf("%w: claude: HTTP 403", integrations.ErrRejected)
	}}
	e.runPoll(u)
	waitFor(t, func() bool {
		list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
		return err == nil && list[0].LastError == "no usage data"
	})
	n := u.n(claudeTok1)
	time.Sleep(30 * time.Millisecond)
	if u.n(claudeTok1) != n {
		t.Fatal("kept polling a rejected setup token")
	}
}

func TestPollUsageSetupTokenRejectedAfterPollFailureStops(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	var offset atomic.Int64
	prevClock := *fleet.Clock
	*fleet.Clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { *fleet.Clock = prevClock })
	e := newEnv(t)
	cl, _, _ := e.claudeAccounts()
	var rejected atomic.Bool
	u := &fakeUsage{fn: func(string) (integrations.Usage, error) {
		if rejected.Load() {
			return integrations.Usage{}, fmt.Errorf("%w: claude: HTTP 403", integrations.ErrRejected)
		}
		return integrations.Usage{}, errors.New("claude: HTTP 500")
	}}
	e.runPoll(u)
	lastError := func() string {
		list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
		if err != nil {
			t.Fatal(err)
		}
		return list[0].LastError
	}
	waitFor(t, func() bool { return lastError() == "usage poll failed" })
	rejected.Store(true)
	offset.Store(int64(time.Hour))
	waitFor(t, func() bool { return lastError() == "no usage data" })
	n := u.n(claudeTok1)
	time.Sleep(30 * time.Millisecond)
	if u.n(claudeTok1) != n {
		t.Fatal("kept polling a rejected setup token")
	}
}

func TestPollUsageErrorShownThenCleared(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	var offset atomic.Int64
	prevClock := *fleet.Clock
	*fleet.Clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { *fleet.Clock = prevClock })
	e := newEnv(t)
	cl, _, _ := e.claudeAccounts()
	var failing atomic.Bool
	failing.Store(true)
	u := &fakeUsage{fn: func(string) (integrations.Usage, error) {
		if failing.Load() {
			return integrations.Usage{}, errors.New("claude: HTTP 500 " + claudeTok1)
		}
		return integrations.Usage{}, nil
	}}
	e.runPoll(u)
	lastError := func() string {
		list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
		if err != nil {
			t.Fatal(err)
		}
		return list[0].LastError
	}
	waitFor(t, func() bool { return lastError() != "" })
	if got := lastError(); got != "usage poll failed" {
		t.Fatalf("last error %q", got)
	}
	failing.Store(false)
	offset.Store(int64(time.Hour))
	waitFor(t, func() bool { return lastError() == "" })
}

func TestUsageReportClampsNodeTimes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	cl, first, _ := e.claudeAccounts()
	e.connect(cred).desired()
	pct := 5.0
	future, far := time.Now().Add(48*time.Hour), time.Now().Add(365*24*time.Hour)
	e.f.NoteUsage(ctx, n, []proto.UsageObservation{{Integration: cl.ID.String(), Account: first.ID.String(), FiveHourPct: &pct, LimitedUntil: &far, ObservedAt: future}})
	list, err := e.s.IntegrationAccounts(ctx, e.org.ID, cl.ID)
	if err != nil || list[0].Usage.At == nil || list[0].Usage.At.After(time.Now()) || list[0].LimitedUntil == nil || list[0].LimitedUntil.After(time.Now().Add(7*24*time.Hour+time.Minute)) {
		t.Fatalf("not clamped: %+v %v", list[0], err)
	}
}

func TestFleetIgnoresMetricsMessage(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "a")
	v := e.vm(e.org.ID, n.ID, "dev", &e.owner, nil)
	node := e.connect(cred)
	node.send(proto.Message{Type: "metrics"})
	node.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning}})
	waitFor(t, func() bool {
		got, err := e.s.VM(context.Background(), e.org.ID, v.ID)
		return err == nil && got.State == proto.StateRunning
	})
}

func TestFleetProcessesRoundTrip(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "a", &e.owner, nil)
	ctx := context.Background()
	if _, err := e.f.Processes(ctx, n.ID, v.ID); !errors.Is(err, fleet.ErrNodeOffline) {
		t.Fatalf("offline: %v", err)
	}
	node := e.connect(cred)
	node.desired()

	type result struct {
		ps  []proto.GuestProcess
		err error
	}
	ask := func(ctx context.Context) chan result {
		out := make(chan result, 1)
		go func() { ps, err := e.f.Processes(ctx, n.ID, v.ID); out <- result{ps, err} }()
		return out
	}
	got := ask(ctx)
	req := node.next()
	if req.Type != "processes" || req.ReqID == "" || req.VM != v.ID.String() {
		t.Fatalf("request %+v", req)
	}
	node.send(proto.Message{Type: "processes", ReqID: "other", Processes: []proto.GuestProcess{{PID: 9}}})
	node.send(proto.Message{Type: "processes", ReqID: req.ReqID, VM: req.VM, Processes: []proto.GuestProcess{{PID: 1, Command: "init"}}})
	if r := <-got; r.err != nil || len(r.ps) != 1 || r.ps[0].Command != "init" {
		t.Fatalf("result %+v", r)
	}

	got = ask(ctx)
	req = node.next()
	node.send(proto.Message{Type: "processes", ReqID: req.ReqID, Error: proto.ErrVMNotRunning})
	if r := <-got; !errors.Is(r.err, fleet.ErrVMNotRunning) {
		t.Fatalf("not running: %+v", r)
	}

	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	got = ask(tctx)
	node.next()
	if r := <-got; !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %+v", r)
	}
	if p := fleet.PendingRequests(e.f, n.ID); p != 0 {
		t.Fatalf("%d pending requests left", p)
	}

	got = ask(ctx)
	node.next()
	e.f.Disconnect(n.ID)
	if r := <-got; !errors.Is(r.err, fleet.ErrNodeOffline) {
		t.Fatalf("disconnect: %+v", r)
	}
}

func TestFleetProcessesCapped(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	v := e.vm(e.org.ID, n.ID, "a", &e.owner, nil)
	node := e.connect(cred)
	node.desired()
	go func() {
		req := <-node.msgs
		_ = node.enc.Encode(proto.Message{Type: "processes", ReqID: req.ReqID, Processes: make([]proto.GuestProcess, 700)})
	}()
	ps, err := e.f.Processes(context.Background(), n.ID, v.ID)
	if err != nil || len(ps) != 500 {
		t.Fatalf("%d entries, %v", len(ps), err)
	}
}
