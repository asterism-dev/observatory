// Package registry issues Docker-token-spec JWTs for the registry and talks to it as the hub.
package registry

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	Service     = "observatory"
	IssuerName  = "observatory-hub"
	TokenTTL    = 5 * time.Minute
	NodePullTTL = 15 * time.Minute
)

// Access is one entry of a token's access claim.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

type Issuer struct {
	Key       *ecdsa.PrivateKey
	CertChain []*x509.Certificate
	Service   string
	Issuer    string
}

type Claims struct {
	jwt.RegisteredClaims
	Access []Access `json:"access"`
}

func (i Issuer) Token(subject string, access []Access, ttl time.Duration) (string, error) {
	if access == nil {
		access = []Access{}
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodES256, Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: i.Issuer, Subject: subject, Audience: jwt.ClaimStrings{i.Service}, ID: rand.Text(),
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}, Access: access})
	x5c := make([]string, len(i.CertChain))
	for k, c := range i.CertChain {
		x5c[k] = base64.StdEncoding.EncodeToString(c.Raw)
	}
	t.Header["x5c"] = x5c
	return t.SignedString(i.Key)
}

// NodePullToken lets a node pull the given repositories of its org.
func (i Issuer) NodePullToken(org uuid.UUID, repos []string) (string, error) {
	access := make([]Access, len(repos))
	for k, r := range repos {
		access[k] = Access{Type: "repository", Name: r, Actions: []string{"pull"}}
	}
	return i.Token("node-org:"+org.String(), access, NodePullTTL)
}

// LoadIssuer reads token.key and token.crt from dir, creating both on first start.
func LoadIssuer(dir string) (Issuer, error) {
	keyPath, certPath := filepath.Join(dir, "token.key"), filepath.Join(dir, "token.crt")
	keyPEM, err := os.ReadFile(keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return createIssuer(keyPath, certPath)
	} else if err != nil {
		return Issuer{}, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return Issuer{}, err
	}
	kb, _ := pem.Decode(keyPEM)
	cb, _ := pem.Decode(certPEM)
	if kb == nil || cb == nil {
		return Issuer{}, fmt.Errorf("registry key dir %s: invalid PEM", dir)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return Issuer{}, fmt.Errorf("%s: %w", keyPath, err)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return Issuer{}, fmt.Errorf("%s: %w", certPath, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return Issuer{}, fmt.Errorf("registry key dir %s: token.crt does not match token.key", dir)
	}
	return newIssuer(key, cert), nil
}

func newIssuer(key *ecdsa.PrivateKey, cert *x509.Certificate) Issuer {
	return Issuer{Key: key, CertChain: []*x509.Certificate{cert}, Service: Service, Issuer: IssuerName}
}

func createIssuer(keyPath, certPath string) (Issuer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Issuer{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return Issuer{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: IssuerName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(20, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Issuer{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Issuer{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Issuer{}, err
	}
	// The certificate goes first so an existing key always has its certificate.
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return Issuer{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return Issuer{}, err
	}
	return newIssuer(key, cert), nil
}
