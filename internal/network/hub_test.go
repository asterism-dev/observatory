package network_test

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

const token = "0123456789abcdef0123456789abcdef"

var cfg = network.Config{GatewayHost: "gw.test", GatewayToken: token}

func newStore(t testing.TB) *store.Store {
	t.Helper()
	return store.New(dbtest.New(t)).WithMasterKey(seal.Key(bytes.Repeat([]byte{1}, 32)))
}

func load(t *testing.T, s *store.Store) *network.Hub {
	t.Helper()
	h, err := network.Load(context.Background(), s, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLoadIsStable(t *testing.T) {
	s := newStore(t)
	a, b := load(t, s), load(t, s)
	if a.ClientsPublicKey() == "" || a.ClientsPublicKey() != b.ClientsPublicKey() {
		t.Fatalf("clients key %q vs %q", a.ClientsPublicKey(), b.ClientsPublicKey())
	}
	ua, ub := underlay(t, s, a), underlay(t, s, b)
	if ua.CACert == "" || ua.CACert != ub.CACert {
		t.Fatal("CA changed between loads")
	}
	if ua.HubKey == "" || ua.HubKey != ub.HubKey || ua.HubKey == a.ClientsPublicKey() {
		t.Fatalf("underlay key %q vs %q", ua.HubKey, ub.HubKey)
	}
	if a.ClientsEndpoint() != "gw.test:51821" {
		t.Fatalf("clients endpoint %q", a.ClientsEndpoint())
	}
}

func TestLoadRejectsConfig(t *testing.T) {
	s := newStore(t)
	for _, c := range []network.Config{{GatewayHost: "gw.test"}, {GatewayHost: "gw.test", GatewayToken: "short"}, {GatewayToken: token}} {
		if _, err := network.Load(context.Background(), s, nil, c); err == nil {
			t.Errorf("%+v loaded", c)
		}
	}
}

func underlayNode(t *testing.T, s *store.Store) store.Node {
	t.Helper()
	ctx := context.Background()
	u, err := s.Login(ctx, store.Identity{Subject: "u", Email: "u@x.io", Name: "u"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, uuid.NewString(), u.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n", CreatedBy: u.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func underlay(t *testing.T, s *store.Store, h *network.Hub) *proto.Underlay {
	t.Helper()
	n := underlayNode(t, s)
	_, pub, _ := wgkey.Generate()
	u, err := h.Underlay(context.Background(), n, pub, csrPEM(csrDER(t, "x")))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUnderlay(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := load(t, s)
	n := underlayNode(t, s)
	_, pub, _ := wgkey.Generate()
	u, err := h.Underlay(ctx, n, pub, csrPEM(csrDER(t, "x")))
	if err != nil {
		t.Fatal(err)
	}
	p, err := netip.ParsePrefix(u.Address)
	if err != nil || p.Bits() != 16 || !netip.MustParsePrefix("10.198.0.0/16").Contains(p.Addr()) {
		t.Fatalf("address %q %v", u.Address, err)
	}
	if u.Endpoint != "gw.test:51820" || u.SBRemote != "ssl:10.198.0.1:6642" || u.CACert == "" {
		t.Fatalf("underlay %+v", u)
	}
	if cn := parseCert(t, u.Cert).Subject.CommonName; cn != n.ID.String() {
		t.Fatalf("cert CN %q", cn)
	}
	_, pub2, _ := wgkey.Generate()
	again, err := h.Underlay(ctx, n, pub2, csrPEM(csrDER(t, "x")))
	if err != nil {
		t.Fatal(err)
	}
	if again.Address != u.Address || again.Cert == "" || again.CACert == "" {
		t.Fatalf("re-issue %+v, first %+v", again, u)
	}
	for name, key := range map[string]string{"empty": "", "garbage": "nope", "short": "AAAA"} {
		if _, err := h.Underlay(ctx, n, key, csrPEM(csrDER(t, "x"))); err == nil {
			t.Errorf("chassis key %s accepted", name)
		}
	}
	if _, err := h.Underlay(ctx, n, pub, "junk"); err == nil {
		t.Error("bad CSR accepted")
	}
}

func TestTokenOK(t *testing.T) {
	h := load(t, newStore(t))
	if !h.TokenOK(token) {
		t.Fatal("right token rejected")
	}
	for _, bad := range []string{"", token[:10], token + "x", strings.Repeat("a", len(token))} {
		if h.TokenOK(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
