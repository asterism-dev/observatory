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
	"fmt"
	"math/big"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	CAValidity    = 30 * 24 * time.Hour
	CARenewBefore = 15 * 24 * time.Hour
	LeafValidity  = 24 * time.Hour
	backdate      = time.Hour
)

var ErrNoCA = errors.New("no CA covers host")

type MintedCA struct {
	CertPEM, KeyPEM     string
	NotBefore, NotAfter time.Time
}

func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func (MintedCA) String() string   { return "MintedCA{redacted}" }
func (MintedCA) GoString() string { return "MintedCA{redacted}" }

func NewCA(hosts []string, now time.Time) (MintedCA, error) {
	if len(hosts) == 0 {
		return MintedCA{}, fmt.Errorf("%w: a CA needs at least one host", ErrInvalidHost)
	}
	if len(hosts) > MaxCAHosts {
		return MintedCA{}, fmt.Errorf("%w: at most %d hosts per CA", ErrInvalidHost, MaxCAHosts)
	}
	for _, h := range hosts {
		if err := validHost(h); err != nil {
			return MintedCA{}, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return MintedCA{}, err
	}
	sn, err := serial()
	if err != nil {
		return MintedCA{}, err
	}
	_, all4, _ := net.ParseCIDR("0.0.0.0/0")
	_, all6, _ := net.ParseCIDR("::/0")
	tmpl := &x509.Certificate{
		SerialNumber:                sn,
		Subject:                     pkix.Name{CommonName: "Observatory egress CA"},
		NotBefore:                   now.Add(-backdate),
		NotAfter:                    now.Add(CAValidity),
		KeyUsage:                    x509.KeyUsageCertSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
		ExcludedIPRanges:            []*net.IPNet{all4, all6},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return MintedCA{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return MintedCA{}, err
	}
	return MintedCA{
		CertPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:    string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		NotBefore: tmpl.NotBefore,
		NotAfter:  tmpl.NotAfter,
	}, nil
}

type issuerCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

type cachedLeaf struct {
	leaf    *tls.Certificate
	expires time.Time
}

type Issuer struct {
	cas   []issuerCA
	now   func() time.Time
	mu    sync.Mutex
	cache map[string]cachedLeaf
}

func NewIssuer(cas []proto.EgressCA, now func() time.Time) (*Issuer, error) {
	iss := &Issuer{now: now, cache: map[string]cachedLeaf{}}
	for _, c := range cas {
		cb, _ := pem.Decode([]byte(c.CertPEM))
		kb, _ := pem.Decode([]byte(c.KeyPEM))
		if cb == nil || kb == nil {
			return nil, fmt.Errorf("egress CA %s: invalid PEM", c.ID)
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			return nil, fmt.Errorf("egress CA %s: %w", c.ID, err)
		}
		key, err := x509.ParseECPrivateKey(kb.Bytes)
		if err != nil {
			return nil, fmt.Errorf("egress CA %s: invalid key", c.ID)
		}
		if !cert.IsCA || !key.PublicKey.Equal(cert.PublicKey) {
			return nil, fmt.Errorf("egress CA %s: not a CA or key does not match", c.ID)
		}
		iss.cas = append(iss.cas, issuerCA{cert, key})
	}
	sort.SliceStable(iss.cas, func(i, j int) bool { return iss.cas[i].cert.NotBefore.Before(iss.cas[j].cert.NotBefore) })
	return iss, nil
}

func (i *Issuer) Leaf(host string) (*tls.Certificate, error) {
	now := i.now()
	i.mu.Lock()
	defer i.mu.Unlock()
	if c, ok := i.cache[host]; ok && now.Before(c.expires) {
		return c.leaf, nil
	}
	var pick *issuerCA
	for n := range i.cas {
		ca := &i.cas[n]
		if !now.Before(ca.cert.NotAfter) || !covers(ca.cert, host) {
			continue
		}
		if pick == nil {
			pick = ca
		}
		if ca.cert.NotAfter.Sub(now) >= time.Hour {
			if pick.cert.NotAfter.Sub(now) < time.Hour {
				pick = ca
			}
			break
		}
	}
	if pick == nil {
		return nil, ErrNoCA
	}
	leaf, err := i.issue(*pick, host, now)
	if err != nil {
		return nil, err
	}
	i.cache[host] = cachedLeaf{leaf, leaf.Leaf.NotAfter.Add(-time.Hour)}
	return leaf, nil
}

func covers(ca *x509.Certificate, host string) bool {
	for _, d := range ca.PermittedDNSDomains {
		if d == host {
			return true
		}
	}
	return false
}

func (i *Issuer) issue(ca issuerCA, host string, now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-backdate),
		NotAfter:     minTime(now.Add(LeafValidity), ca.cert.NotAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
