package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
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
