package guestinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testPEM   = "-----BEGIN CERTIFICATE-----\nMIIBdGVzdA==\n-----END CERTIFICATE-----\n"
	testKey   = "-----BEGIN EC PRIVATE KEY-----\nMHcC\n-----END EC PRIVATE KEY-----\n"
	caID1     = "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01"
	caID2     = "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e02"
	debBundle = "etc/ssl/certs/ca-certificates.crt"
	fedBundle = "etc/pki/tls/certs/ca-bundle.crt"
)

func TestInstallCAsWritesBlockAndKeepsBundle(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, debBundle, "ROOTS\n", 0o644)
	bundle, err := r.InstallCAs(map[string]string{caID1: testPEM})
	if err != nil || bundle != "/"+debBundle {
		t.Fatalf("bundle = %q, %v", bundle, err)
	}
	want := "ROOTS\n# BEGIN observatory egress\n" + testPEM + "# END observatory egress\n"
	if got := get(t, r, debBundle); got != want {
		t.Fatalf("bundle = %q, want %q", got, want)
	}
	if st, _ := os.Stat(filepath.Join(r.Path, debBundle)); st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", st.Mode())
	}
	again, _ := r.InstallCAs(map[string]string{caID1: testPEM})
	if again != bundle || get(t, r, debBundle) != want {
		t.Fatal("second install is not idempotent")
	}
}

func TestInstallCAsReplacesAndRemovesBlock(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, debBundle, "ROOTS\n", 0o644)
	r.InstallCAs(map[string]string{caID1: testPEM})
	other := strings.Replace(testPEM, "dGVzdA", "b3RoZXI", 1)
	if _, err := r.InstallCAs(map[string]string{caID2: other}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, debBundle); strings.Count(got, "BEGIN observatory") != 1 || !strings.Contains(got, "b3RoZXI") || strings.Contains(got, "dGVzdA") {
		t.Fatalf("bundle = %q", got)
	}
	if _, err := r.InstallCAs(nil); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, debBundle); got != "ROOTS\n" {
		t.Fatalf("bundle after removal = %q", got)
	}
}

func TestInstallCAsRestoresBlockAfterBundleRewrite(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, debBundle, "ROOTS\n", 0o644)
	certs := map[string]string{caID1: testPEM}
	r.InstallCAs(certs)
	put(t, r, debBundle, "NEWROOTS\n", 0o644)
	if _, err := r.InstallCAs(certs); err != nil {
		t.Fatal(err)
	}
	if got, want := get(t, r, debBundle), "NEWROOTS\n# BEGIN observatory egress\n"+testPEM+"# END observatory egress\n"; got != want {
		t.Fatalf("bundle = %q, want %q", got, want)
	}
}

func TestInstallCAsFedoraPath(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, fedBundle, "ROOTS", 0o644)
	bundle, err := r.InstallCAs(map[string]string{caID1: testPEM})
	if err != nil || bundle != "/"+fedBundle {
		t.Fatalf("bundle = %q, %v", bundle, err)
	}
	if got, want := get(t, r, fedBundle), "ROOTS\n# BEGIN observatory egress\n"+testPEM+"# END observatory egress\n"; got != want {
		t.Fatalf("bundle = %q, want %q", got, want)
	}
	if exists(r, debBundle) {
		t.Fatal("created the Debian bundle next to an existing one")
	}
}

func TestInstallCAsCreatesBundleWhenNoneExists(t *testing.T) {
	r := Root{t.TempDir()}
	bundle, err := r.InstallCAs(map[string]string{caID1: testPEM})
	if err != nil || bundle != "/"+debBundle {
		t.Fatalf("bundle = %q, %v", bundle, err)
	}
	if got := get(t, r, debBundle); !strings.Contains(got, testPEM) {
		t.Fatalf("bundle = %q", got)
	}
	empty := Root{t.TempDir()}
	if _, err := empty.InstallCAs(nil); err != nil || exists(empty, debBundle) {
		t.Fatalf("bundle created without certs: %v", err)
	}
}

func TestInstallCAsWritesThroughSymlinkInsideRoot(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "ROOTS\n", 0o644)
	if err := os.MkdirAll(filepath.Join(r.Path, filepath.Dir(fedBundle)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", filepath.Join(r.Path, fedBundle)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.InstallCAs(map[string]string{caID1: testPEM}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"); !strings.Contains(got, testPEM) {
		t.Fatalf("target = %q", got)
	}
	if fi, _ := os.Lstat(filepath.Join(r.Path, fedBundle)); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
}

func TestInstallCAsRejectsPrivateKey(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, debBundle, "ROOTS\n", 0o644)
	for name, certs := range map[string]map[string]string{
		"key":        {caID1: testKey},
		"cert+key":   {caID1: testPEM + testKey},
		"junk":       {caID1: "x"},
		"empty":      {caID1: ""},
		"bad id":     {"../x": testPEM},
		"upper id":   {strings.ToUpper(caID1): testPEM},
		"oversize":   {caID1: strings.Repeat(testPEM, 400)},
		"good+bad":   {caID2: testPEM, caID1: testKey},
		"short id":   {"abc": testPEM},
		"trailing x": {caID1: testPEM + "x"},
	} {
		if _, err := r.InstallCAs(certs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := get(t, r, debBundle); got != "ROOTS\n" {
		t.Fatalf("rejected request changed the bundle: %q", got)
	}
}
