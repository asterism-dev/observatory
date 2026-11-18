package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func mustCA(t *testing.T, now time.Time, hosts ...string) MintedCA {
	t.Helper()
	ca, err := NewCA(hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func parseCA(t *testing.T, ca MintedCA) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	cb, _ := pem.Decode([]byte(ca.CertPEM))
	kb, _ := pem.Decode([]byte(ca.KeyPEM))
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func rogueLeaf(t *testing.T, ca MintedCA, dns string, ip net.IP) *tls.Certificate {
	t.Helper()
	caCert, caKey := parseCA(t, ca)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "rogue"},
		NotBefore:    t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if dns != "" {
		tmpl.DNSNames = []string{dns}
	}
	if ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func pool(t *testing.T, ca MintedCA) *x509.CertPool {
	c, _ := parseCA(t, ca)
	p := x509.NewCertPool()
	p.AddCert(c)
	return p
}

func verify(t *testing.T, leaf *tls.Certificate, roots *x509.CertPool, dns string, at time.Time) error {
	t.Helper()
	c, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Verify(x509.VerifyOptions{Roots: roots, DNSName: dns, CurrentTime: at})
	return err
}

func issuerFor(t *testing.T, now time.Time, cas ...MintedCA) *Issuer {
	t.Helper()
	var in []proto.EgressCA
	for i, ca := range cas {
		in = append(in, proto.EgressCA{ID: strconv.Itoa(i), CertPEM: ca.CertPEM, KeyPEM: ca.KeyPEM})
	}
	iss, err := NewIssuer(in, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

func TestCANameConstraints(t *testing.T) {
	ca := mustCA(t, t0, "a.test")
	cert, _ := parseCA(t, ca)
	if !cert.IsCA || !cert.MaxPathLenZero || !cert.PermittedDNSDomainsCritical ||
		len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != "a.test" {
		t.Fatalf("bad constraints: %+v", cert)
	}
	var ex []string
	for _, n := range cert.ExcludedIPRanges {
		ex = append(ex, n.String())
	}
	if len(ex) != 2 || ex[0] != "0.0.0.0/0" || ex[1] != "::/0" {
		t.Fatalf("excluded %v", ex)
	}
	if k, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		t.Fatal("not P-256")
	}
	roots := pool(t, ca)
	leaf, err := issuerFor(t, t0, ca).Leaf("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, leaf, roots, "a.test", t0); err != nil {
		t.Fatal(err)
	}
	if err := verify(t, rogueLeaf(t, ca, "evil.test", nil), roots, "evil.test", t0); err == nil {
		t.Fatal("evil.test verified")
	}
	if err := verify(t, rogueLeaf(t, ca, "", net.ParseIP("1.2.3.4")), roots, "", t0); err == nil {
		t.Fatal("IP SAN verified")
	}
}

func writePEM(t *testing.T, dir, name string, der []byte) string {
	p := filepath.Join(dir, name)
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCAConstraintsOpenSSLNode(t *testing.T) {
	ca := mustCA(t, time.Now(), "a.test")
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, []byte(ca.CertPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	evil := rogueLeaf(t, ca, "evil.test", nil)
	ipLeaf := rogueLeaf(t, ca, "", net.ParseIP("127.0.0.1"))

	if path, err := exec.LookPath("openssl"); err != nil {
		t.Log("openssl not found, skipping that part")
	} else {
		f := writePEM(t, dir, "evil.pem", evil.Certificate[0])
		if err := exec.Command(path, "verify", "-CAfile", caFile, f).Run(); err == nil {
			t.Error("openssl accepted evil.test leaf")
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	script := `const https=require('https'),fs=require('fs');
https.get({host:'127.0.0.1',port:+process.argv[1],servername:process.argv[2],ca:fs.readFileSync(process.argv[3]),agent:false},
 r=>{console.log('connected');process.exit(0)}).on('error',e=>{process.exit(1)});`
	for name, c := range map[string]*tls.Certificate{"evil.test": evil, "": ipLeaf} {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*c}})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { conn.(*tls.Conn).Handshake(); conn.Close() }()
			}
		}()
		port := ln.Addr().(*net.TCPAddr).Port
		sni := name
		if sni == "" {
			sni = "a.test"
		}
		cmd := exec.Command(node, "-e", script, strconv.Itoa(port), sni, caFile)
		if err := cmd.Run(); err == nil {
			t.Errorf("node accepted leaf %q", name)
		}
		ln.Close()
	}
}

func TestCABackdated(t *testing.T) {
	ca := mustCA(t, t0, "a.test")
	if !ca.NotBefore.Equal(t0.Add(-time.Hour)) || !ca.NotAfter.Equal(t0.Add(30*24*time.Hour)) {
		t.Fatalf("%v %v", ca.NotBefore, ca.NotAfter)
	}
	leaf, err := issuerFor(t, t0, ca).Leaf("a.test")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(leaf.Certificate[0])
	if c.NotBefore.After(t0.Add(-time.Hour + time.Second)) {
		t.Fatalf("not backdated: %v", c.NotBefore)
	}
	if !c.NotAfter.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("NotAfter %v", c.NotAfter)
	}
	if len(c.DNSNames) != 1 || c.DNSNames[0] != "a.test" || len(c.IPAddresses) != 0 {
		t.Fatalf("SAN %v %v", c.DNSNames, c.IPAddresses)
	}
}

func TestIssuerOldestCovering(t *testing.T) {
	ca1 := mustCA(t, t0.Add(-48*time.Hour), "a.test")
	ca2 := mustCA(t, t0, "a.test", "b.test")
	iss := issuerFor(t, t0, ca1, ca2)
	a, err := iss.Leaf("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, a, pool(t, ca1), "a.test", t0); err != nil {
		t.Fatalf("a.test not from CA1: %v", err)
	}
	b, err := iss.Leaf("b.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, b, pool(t, ca2), "b.test", t0); err != nil {
		t.Fatalf("b.test not from CA2: %v", err)
	}
	if _, err := iss.Leaf("c.test"); !errors.Is(err, ErrNoCA) {
		t.Fatalf("c.test: %v", err)
	}

	late := t0.Add(30*24*time.Hour - 48*time.Hour + time.Minute)
	iss = issuerFor(t, late, ca1, ca2)
	a, err = iss.Leaf("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, a, pool(t, ca2), "a.test", late); err != nil {
		t.Fatalf("a.test after CA1 expiry not from CA2: %v", err)
	}
}
