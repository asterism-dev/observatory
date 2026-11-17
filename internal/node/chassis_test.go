package node

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateChassisKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chassis.key")
	priv, pub, err := LoadOrCreateChassisKey(path)
	if err != nil || priv == "" || pub == "" {
		t.Fatalf("create: %q %q %v", priv, pub, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat: %v %v", st, err)
	}
	priv2, pub2, err := LoadOrCreateChassisKey(path)
	if err != nil || priv2 != priv || pub2 != pub {
		t.Fatalf("reload: %q %q %v", priv2, pub2, err)
	}
}

func TestLoadOrCreateChassisKeyRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	trunc := filepath.Join(dir, "trunc.key")
	os.WriteFile(trunc, []byte("abc"), 0o600)
	if _, _, err := LoadOrCreateChassisKey(trunc); err == nil || !strings.Contains(err.Error(), trunc) {
		t.Fatalf("truncated key: err = %v", err)
	}
	loose := filepath.Join(dir, "loose.key")
	if _, _, err := LoadOrCreateChassisKey(loose); err != nil {
		t.Fatal(err)
	}
	os.Chmod(loose, 0o644)
	if _, _, err := LoadOrCreateChassisKey(loose); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("loose key: err = %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestOVNKeyCSR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ovn-key.pem")
	csrPEM, err := LoadOrCreateOVNKey(path, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat: %v %v", st, err)
	}
	csr := parseCSR(t, csrPEM)
	if err := csr.CheckSignature(); err != nil {
		t.Fatal(err)
	}
	if csr.Subject.CommonName != "node-1" {
		t.Fatalf("CN = %q", csr.Subject.CommonName)
	}
	again, err := LoadOrCreateOVNKey(path, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !parseCSR(t, again).PublicKey.(*ecdsa.PublicKey).Equal(csr.PublicKey) {
		t.Fatal("second call changed the key")
	}
}

func parseCSR(t *testing.T, s string) *x509.CertificateRequest {
	t.Helper()
	b, _ := pem.Decode([]byte(s))
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("not a CSR PEM: %q", s)
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}
