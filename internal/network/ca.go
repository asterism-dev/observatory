// Package network holds the hub's network keys, the OVN CA and node underlay issuance.
package network

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

const (
	caValidity      = 10 * 365 * 24 * time.Hour
	chassisValidity = 365 * 24 * time.Hour
	clockSkew       = 5 * time.Minute
)

var errBundle = errors.New("invalid certificate bundle")

// CA is the hub-held OVN certificate authority that signs chassis and southbound server certs.
type CA struct {
	cert    *x509.Certificate
	certPEM string
	key     crypto.Signer
}

// NewCA returns a PEM bundle (certificate, then PKCS#8 key) of a new self-signed CA.
func NewCA() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl, err := template(pkix.Name{CommonName: "observatory OVN CA"}, caValidity)
	if err != nil {
		return nil, err
	}
	tmpl.IsCA = true
	tmpl.BasicConstraintsValid = true
	tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := encode(der, key)
	if err != nil {
		return nil, err
	}
	return append([]byte(certPEM), keyPEM...), nil
}

func ParseCA(bundle []byte) (*CA, error) {
	cert, key, err := parseBundle(bundle)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("%w: not a CA", errBundle)
	}
	return &CA{cert: cert, certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})), key: key}, nil
}

func (c *CA) CertPEM() string { return c.certPEM }

// SignChassis issues a client cert for the CSR's key; the subject is the node id, whatever the CSR asked for.
func (c *CA) SignChassis(node uuid.UUID, csrPEM string) (string, error) {
	b, _ := pem.Decode([]byte(csrPEM))
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return "", errors.New("chassis csr: no PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return "", fmt.Errorf("chassis csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("chassis csr: %w", err)
	}
	if k, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		return "", errors.New("chassis csr: key must be ECDSA P-256")
	}
	tmpl, err := template(pkix.Name{CommonName: node.String()}, chassisValidity)
	if err != nil {
		return "", err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

// ServerCert issues a server cert and fresh key for ip; it lives as long as the CA because it is stored once.
func (c *CA) ServerCert(ip netip.Addr) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	tmpl, err := template(pkix.Name{CommonName: ip.String()}, time.Until(c.cert.NotAfter))
	if err != nil {
		return "", "", err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	tmpl.IPAddresses = []net.IP{ip.AsSlice()}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return "", "", err
	}
	return encode(der, key)
}

func template(subject pkix.Name, validity time.Duration) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &x509.Certificate{SerialNumber: serial, Subject: subject, NotBefore: now.Add(-clockSkew), NotAfter: now.Add(validity)}, nil
}

func encode(certDER []byte, key *ecdsa.PrivateKey) (certPEM, keyPEM string, err error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// parseBundle reads a certificate and its matching private key; errors never include key material.
func parseBundle(bundle []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, rest := pem.Decode(bundle)
	keyBlock, _ := pem.Decode(rest)
	if certBlock == nil || keyBlock == nil || certBlock.Type != "CERTIFICATE" || keyBlock.Type != "PRIVATE KEY" {
		return nil, nil, errBundle
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errBundle, err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, errBundle
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("%w: key does not match certificate", errBundle)
	}
	return cert, key, nil
}
