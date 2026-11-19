package network_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/network"
)

func newCA(t *testing.T) *network.CA {
	t.Helper()
	bundle, err := network.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := network.ParseCA(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func csrDER(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func csrPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func parseCert(t *testing.T, s string) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		t.Fatalf("no PEM in %q", s)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func verify(t *testing.T, ca *network.CA, cert *x509.Certificate, usage x509.ExtKeyUsage) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(parseCert(t, ca.CertPEM()))
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		t.Fatal(err)
	}
}

func TestSignChassisCN(t *testing.T) {
	ca := newCA(t)
	node := uuid.New()
	certPEM, err := ca.SignChassis(node, csrPEM(csrDER(t, "evil")))
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, certPEM)
	if cert.Subject.CommonName != node.String() {
		t.Fatalf("CN %q, want %s", cert.Subject.CommonName, node)
	}
	verify(t, ca, cert, x509.ExtKeyUsageClientAuth)
}

func TestSignChassisRejectsBadCSR(t *testing.T) {
	ca := newCA(t)
	der := csrDER(t, "n")
	der[len(der)-3] ^= 0xff
	for name, in := range map[string]string{"garbage": "not a csr", "bad signature": csrPEM(der)} {
		if _, err := ca.SignChassis(uuid.New(), in); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
}

func TestServerCert(t *testing.T) {
	ca := newCA(t)
	ip := netip.MustParseAddr("10.198.0.1")
	certPEM, keyPEM, err := ca.ServerCert(ip)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := pem.Decode([]byte(keyPEM)); b == nil {
		t.Fatal("no key PEM")
	}
	cert := parseCert(t, certPEM)
	verify(t, ca, cert, x509.ExtKeyUsageServerAuth)
	if err := cert.VerifyHostname(ip.String()); err != nil {
		t.Fatal(err)
	}
}
