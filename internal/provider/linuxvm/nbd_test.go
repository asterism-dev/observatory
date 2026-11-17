package linuxvm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseNBDURL(t *testing.T) {
	host, port, export, err := parseNBDURL("nbd://10.0.0.5:10809/disk-1.img")
	if err != nil || host != "10.0.0.5" || port != "10809" || export != "disk-1.img" {
		t.Fatalf("got %q %q %q %v", host, port, export, err)
	}
	for _, bad := range []string{"nbd://10.0.0.5/disk.img", "nbd://10.0.0.5:10809/", "http://h:1/x"} {
		if _, _, _, err := parseNBDURL(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestQemuNBDArgs(t *testing.T) {
	got := qemuNBDArgs("/dev/nbd3", "10.0.0.5", "10809", "disk-1.img", "/v/nbd-disk-1.img-tls")
	want := []string{"--connect=/dev/nbd3",
		"--object", "tls-creds-psk,id=tls0,endpoint=client,dir=/v/nbd-disk-1.img-tls,username=disk-1.img", "--image-opts",
		"driver=raw,file.driver=nbd,file.server.type=inet,file.server.host=10.0.0.5,file.server.port=10809,file.export=disk-1.img,file.reconnect-delay=604800,file.tls-creds=tls0"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
	// image-opts separates options with commas; one inside a value must be doubled.
	if got := qemuNBDArgs("/dev/nbd0", "h", "1", "a,b.img", "/t")[4]; !strings.Contains(got, "file.export=a,,b.img,") {
		t.Errorf("comma in the export name is not escaped: %s", got)
	}
}

func TestQemuNBDArgsTLS(t *testing.T) {
	psk := strings.Repeat("ab", 32)
	got := qemuNBDArgs("/dev/nbd0", "10.198.0.1", "10810", "d1", "/v/nbd-d1-tls")
	i := slices.Index(got, "--object")
	if i < 0 || i+1 >= len(got) || got[i+1] != "tls-creds-psk,id=tls0,endpoint=client,dir=/v/nbd-d1-tls,username=d1" {
		t.Fatalf("no tls-creds-psk object: %q", got)
	}
	if !strings.HasSuffix(got[len(got)-1], "file.reconnect-delay=604800,file.tls-creds=tls0") {
		t.Errorf("image opts = %s", got[len(got)-1])
	}
	for _, a := range got {
		if strings.Contains(a, psk) {
			t.Errorf("key in args: %s", a)
		}
	}
}

func TestAttachNBDRejectsBadPSK(t *testing.T) {
	for _, psk := range []string{"", "zz", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		dir := t.TempDir()
		if _, err := writeNBDKey(dir, "d1", psk); err == nil {
			t.Errorf("psk %q accepted", psk)
		}
		if _, err := os.Stat(filepath.Join(dir, "nbd-d1-tls", "keys.psk")); err == nil {
			t.Errorf("psk %q: keys.psk written", psk)
		}
	}
}

func TestWriteNBDKey(t *testing.T) {
	dir := t.TempDir()
	psk := strings.Repeat("0f", 32)
	tlsDir, err := writeNBDKey(dir, "d1", psk)
	if err != nil || tlsDir != filepath.Join(dir, "nbd-d1-tls") {
		t.Fatalf("writeNBDKey = %s, %v", tlsDir, err)
	}
	b, err := os.ReadFile(filepath.Join(tlsDir, "keys.psk"))
	if err != nil || string(b) != "d1:"+psk+"\n" {
		t.Fatalf("keys.psk = %q, %v", b, err)
	}
	for path, want := range map[string]os.FileMode{tlsDir: 0o700, filepath.Join(tlsDir, "keys.psk"): 0o600} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", path, st.Mode().Perm(), want)
		}
	}
}

func TestParseNBDRecord(t *testing.T) {
	for in, want := range map[string][2]string{
		"/dev/nbd3\nnbd://10.198.0.1:10810/d1\n": {"/dev/nbd3", "nbd://10.198.0.1:10810/d1"},
		"/dev/nbd3\n":                            {"/dev/nbd3", ""},
		"":                                       {"", ""},
	} {
		if dev, url := parseNBDRecord([]byte(in)); dev != want[0] || url != want[1] {
			t.Errorf("%q: %q %q", in, dev, url)
		}
	}
}
