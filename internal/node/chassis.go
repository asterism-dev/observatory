package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asterism-dev/observatory/internal/wgkey"
)

func LoadOrCreateChassisKey(path string) (priv, pub string, err error) {
	st, err := os.Stat(path)
	if err == nil {
		if st.Mode().Perm()&0o077 != 0 {
			return "", "", fmt.Errorf("chassis key %s has permissions %o, want 0600", path, st.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", "", err
		}
		priv = strings.TrimSpace(string(b))
		if pub, err = wgkey.Public(priv); err != nil {
			return "", "", fmt.Errorf("chassis key %s is invalid (delete it to generate a new one): %w", path, err)
		}
		return priv, pub, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if priv, pub, err = wgkey.Generate(); err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(path, []byte(priv+"\n")); err != nil {
		return "", "", err
	}
	return priv, pub, nil
}

// LoadOrCreateOVNKey keeps the OVN client key across restarts and returns a fresh CSR for it with CN = nodeID.
func LoadOrCreateOVNKey(path, nodeID string) (csrPEM string, err error) {
	key, err := loadOVNKey(path)
	if errors.Is(err, os.ErrNotExist) {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return "", err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return "", err
		}
		if err := writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID}}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), nil
}

func loadOVNKey(path string) (*ecdsa.PrivateKey, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("ovn key %s has permissions %o, want 0600", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	invalid := fmt.Errorf("ovn key %s is invalid (delete it to generate a new one)", path)
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, invalid
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", invalid, err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, invalid
	}
	return key, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
