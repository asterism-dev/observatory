package fleet_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/wgkey"
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
	reg.NodeID, reg.Hostname, reg.CPUs, reg.MemoryMB, reg.Version = "whatever", "h", 4, 4096, "v1"
	n.send(proto.Message{Type: "register", Register: &reg})
	return n
}

func chassisCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "spoofed"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
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

// desired skips underlay messages.
func (n *fakeNode) desired() proto.Message {
	n.t.Helper()
	for {
		m := n.next()
		if m.Type == "underlay" {
			continue
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

func TestUnderlayOnRegister(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "vm", &e.owner, nil)
	_, pub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}

	node := e.register(cred, proto.Register{ChassisKey: pub, ChassisCSR: chassisCSR(t)})
	m := node.next()
	if m.Type != "underlay" || m.Underlay == nil {
		t.Fatalf("first message %q", m.Type)
	}
	if cn := certCN(t, m.Underlay.Cert); cn != n.ID.String() {
		t.Errorf("cert CN = %q, want %s", cn, n.ID)
	}
	if m.Underlay.CACert == "" || m.Underlay.Address == "" {
		t.Errorf("incomplete underlay %+v", m.Underlay)
	}
	if m := node.next(); m.Type != "desired" {
		t.Fatalf("second message %q", m.Type)
	}
	node.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(n.ID) })

	for _, reg := range []proto.Register{{}, {ChassisKey: pub}, {ChassisCSR: chassisCSR(t)}, {ChassisKey: "bogus", ChassisCSR: chassisCSR(t)}} {
		node := e.register(cred, reg)
		if m := node.next(); m.Type != "desired" {
			t.Errorf("register %+v: first message %q", reg, m.Type)
		}
		node.sess.Close()
		waitFor(t, func() bool { return !e.f.Online(n.ID) })
	}
}

func TestUnderlayRenewed(t *testing.T) {
	check, before := *fleet.UnderlayCheck, *fleet.UnderlayRenewBefore
	*fleet.UnderlayCheck, *fleet.UnderlayRenewBefore = 50*time.Millisecond, 400*24*time.Hour
	t.Cleanup(func() { *fleet.UnderlayCheck, *fleet.UnderlayRenewBefore = check, before })
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	_, pub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	node := e.register(cred, proto.Register{ChassisKey: pub, ChassisCSR: chassisCSR(t)})
	first := node.next()
	if first.Type != "underlay" {
		t.Fatalf("first message %q", first.Type)
	}
	for {
		m := node.next()
		if m.Type != "underlay" {
			continue
		}
		if m.Underlay.Cert == first.Underlay.Cert || certCN(t, m.Underlay.Cert) != n.ID.String() {
			t.Fatalf("renewed cert not fresh or wrong CN")
		}
		return
	}
}

func TestUnderlayRetriedAfterFailure(t *testing.T) {
	old := *fleet.DesiredRetry
	*fleet.DesiredRetry = 50 * time.Millisecond
	t.Cleanup(func() { *fleet.DesiredRetry = old })
	ctx := context.Background()
	e := newEnv(t)
	_, cred := e.node(e.org.ID, "n")
	other, _ := e.node(e.org.ID, "other")
	_, pub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// Another node holding the key makes enrollment fail until it lets go.
	if _, err := e.s.EnrollChassis(ctx, other.ID, pub); err != nil {
		t.Fatal(err)
	}
	node := e.register(cred, proto.Register{ChassisKey: pub, ChassisCSR: chassisCSR(t)})
	if m := node.next(); m.Type != "desired" {
		t.Fatalf("first message %q", m.Type)
	}
	_, otherPub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EnrollChassis(ctx, other.ID, otherPub); err != nil {
		t.Fatal(err)
	}
	for m := node.next(); m.Type != "underlay"; m = node.next() {
	}
}

func certCN(t *testing.T, certPEM string) string {
	t.Helper()
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		t.Fatal("no PEM cert")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.Subject.CommonName
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
	if err := e.s.DeleteVM(ctx, e.org.ID, gone.ID); err != nil {
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
		r := node.desired().Desired[0].Restore
		if r == nil || *r != (proto.Restore{SnapshotID: sn.ID.String(), Seq: 1, Ready: c.ready}) {
			t.Fatalf("%s: restore = %+v", c.state, r)
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
