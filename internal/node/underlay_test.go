package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestAgentFetchesUnderlayBeforeConnect(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) {
		a.ChassisKey, a.ChassisCSR, a.ChassisPrivateKey, a.OVNKeyFile = "pub-key", "CSR PEM", "priv-key", "/vms/ovn-key.pem"
	})
	env.hub.mu.Lock()
	events, reqs := slices.Clone(env.hub.events), slices.Clone(env.hub.underlayReqs)
	env.hub.mu.Unlock()
	if len(events) < 2 || events[0] != "underlay" || events[1] != "connect" {
		t.Fatalf("events = %v", events)
	}
	if len(reqs) != 1 || reqs[0].ChassisKey != "pub-key" || reqs[0].ChassisCSR != "CSR PEM" {
		t.Fatalf("underlay requests = %+v", reqs)
	}
	eventually(t, "chassis started", func() bool { return len(env.provider.gotChassis()) == 1 })
	if c, u := env.provider.gotChassis()[0], env.hub.underlay; c.Address != u.Address || c.Cert != u.Cert || c.PrivateKey != "priv-key" {
		t.Fatalf("chassis = %+v", c)
	}
}

func TestAgentWithoutChassisSkipsUnderlay(t *testing.T) {
	env := startEnv(t, nil)
	env.hub.mu.Lock()
	defer env.hub.mu.Unlock()
	if len(env.hub.underlayReqs) != 0 {
		t.Fatalf("underlay requests = %+v", env.hub.underlayReqs)
	}
}

func certWithValidity(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "n"}, NotBefore: notAfter.Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestCertExpiresSoon(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	if !certExpiresSoon(certWithValidity(t, now.Add(29*day)), now) {
		t.Error("29 days left is not soon")
	}
	if certExpiresSoon(certWithValidity(t, now.Add(31*day)), now) {
		t.Error("31 days left is soon")
	}
	if !certExpiresSoon("garbage", now) {
		t.Error("unparseable cert should renew")
	}
}

func TestAgentConnectsOnlyAfterChassisIsUp(t *testing.T) {
	h := newFakeHub(t)
	p := newFakeProvider()
	p.block = make(chan struct{})
	a := &Agent{HubURL: h.url(), controlBase: h.url(), Credential: "tok", NodeID: "n1", VMsDir: t.TempDir(), CPUs: 4, MemoryMB: 8192, Provider: p,
		ChassisKey: "pub-key", ChassisCSR: "CSR PEM", ChassisPrivateKey: "priv-key", OVNKeyFile: "/vms/ovn-key.pem"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	unblock := sync.OnceFunc(func() { close(p.block) })
	t.Cleanup(unblock)
	eventually(t, "chassis starting", func() bool { return len(p.gotChassis()) == 1 })
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	early := slices.Contains(h.events, "connect")
	h.mu.Unlock()
	if early {
		t.Fatal("dialed the hub before wg-underlay was up")
	}
	unblock()
	eventually(t, "node online", func() bool { return len(h.registers()) > 0 })
}
