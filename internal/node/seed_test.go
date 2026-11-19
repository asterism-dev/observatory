package node

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestWriteSeedISORoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{"user-data": "a", "meta-data": "b", "network-config": "c", "vmm-disks.sh": "d"}
	files := map[string][]byte{}
	for k, v := range want {
		files[k] = []byte(v)
	}
	if err := WriteSeedISO(dir, files); err != nil {
		t.Fatal(err)
	}
	// vz rejects disk images whose size is not a whole number of sectors.
	if st, err := os.Stat(filepath.Join(dir, SeedISO)); err != nil || st.Size()%2048 != 0 {
		t.Fatalf("seed size not a multiple of 2048: %v %v", st.Size(), err)
	}
	d, err := diskfs.Open(filepath.Join(dir, SeedISO), diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(fs.Label(), "\x00 "); got != "cidata" {
		t.Errorf("label = %q, want cidata", got)
	}
	for name, content := range want {
		f, err := fs.OpenFile("/"+name, os.O_RDONLY)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		b, err := io.ReadAll(f)
		if err != nil || string(b) != content {
			t.Errorf("%s = %q, %v; want %q", name, b, err, content)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestSeedBootRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := SeedBoot(dir); err == nil {
		t.Fatal("no error without a seed")
	}
	want := &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAAA one", "ssh-rsa BBBB two"}, Env: map[string]string{"A": "x\ny", "B": `it's \ "q"`, "C": ""}}
	if err := WriteSeedISO(dir, GuestSeedFiles(VMFile{Boot: want}, PrimaryNet{})); err != nil {
		t.Fatal(err)
	}
	got, err := SeedBoot(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("SeedBoot = %+v, %v; want %+v", got, err, want)
	}
	if err := WriteSeedISO(dir, GuestSeedFiles(VMFile{}, PrimaryNet{})); err != nil {
		t.Fatal(err)
	}
	if got, err := SeedBoot(dir); err != nil || len(got.SSHPublicKeys) != 0 || len(got.Env) != 0 {
		t.Fatalf("SeedBoot of an empty seed = %+v, %v", got, err)
	}
}

const testCA = "-----BEGIN CERTIFICATE-----\nMIIBdGVzdA==\n-----END CERTIFICATE-----\n"

func TestSeedBootRecoversEgressCAs(t *testing.T) {
	dir := t.TempDir()
	want := &proto.Boot{EgressCAs: map[string]string{"0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01": testCA}}
	if err := WriteSeedISO(dir, GuestSeedFiles(VMFile{Boot: want}, PrimaryNet{})); err != nil {
		t.Fatal(err)
	}
	if got, err := SeedBoot(dir); err != nil || !reflect.DeepEqual(got.EgressCAs, want.EgressCAs) {
		t.Fatalf("SeedBoot = %+v, %v; want %+v", got, err, want)
	}
}

func guestSeed(t *testing.T, f VMFile, primary PrimaryNet) proto.GuestSeed {
	t.Helper()
	files := GuestSeedFiles(f, primary)
	if len(files) != 1 {
		t.Fatalf("seed files = %v, want only %s", slices.Collect(maps.Keys(files)), proto.GuestSeedFile)
	}
	var seed proto.GuestSeed
	if err := json.Unmarshal(files[proto.GuestSeedFile], &seed); err != nil {
		t.Fatal(err)
	}
	return seed
}

func TestGuestSeedFilesOnlyJSON(t *testing.T) {
	f := VMFile{MAC: "06:00:00:00:00:01", Spec: proto.VMSpec{ID: "v1", Name: "dev",
		NICs:        []proto.NIC{{Network: "lan", MAC: "02:00:00:00:00:02"}, {Network: "mgmt", MAC: "02:00:00:00:00:03"}, {Network: "db", MAC: "02:00:00:00:00:04"}},
		Attachments: []proto.DiskAttachment{{DiskID: "d1", MountPath: "/data"}, {DiskID: "d2", MountPath: "/it's"}}},
		ImageConfig: proto.ImageConfig{Cmd: []string{"sleep", "1"}, Env: []string{"A=b"}},
		Boot:        &proto.Boot{Env: map[string]string{"K": "v"}, EgressCAs: map[string]string{"ca": testCA}}}
	got := guestSeed(t, f, PrimaryNet{})
	want := proto.GuestSeed{Hostname: "dev", InstanceID: "v1", Env: map[string]string{"K": "v"}, EgressCAs: map[string]string{"ca": testCA}, Context: contextFile,
		Image: f.ImageConfig,
		NICs: []proto.GuestNIC{
			{MAC: "06:00:00:00:00:01", Name: "eth0", DHCP: true},
			{MAC: "02:00:00:00:00:02", Name: "net0", DHCP: true},
			{MAC: "02:00:00:00:00:03", Name: "mgmt0", DHCP: true, Mgmt: true},
			{MAC: "02:00:00:00:00:04", Name: "net1", DHCP: true},
		},
		Disks: []proto.GuestDisk{{Device: "/dev/vdb", MountPath: "/data"}, {Device: "/dev/vdc", MountPath: "/it's"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seed = %+v\nwant %+v", got, want)
	}
	static := guestSeed(t, f, PrimaryNet{Address: "192.168.64.5/24", Gateway: "192.168.64.1"}).NICs[0]
	if static != (proto.GuestNIC{MAC: f.MAC, Name: "eth0", Address: "192.168.64.5/24", Gateway: "192.168.64.1"}) {
		t.Fatalf("static primary = %+v", static)
	}
}

func TestSeedKeysTrimmed(t *testing.T) {
	seed := guestSeed(t, VMFile{Boot: &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAAA one@host\n", "", "  \n", "ssh-rsa BB\nssh-rsa CC", " ssh-rsa DD two\r\n"}}}, PrimaryNet{})
	if want := []string{"ssh-ed25519 AAAA one@host", "ssh-rsa DD two"}; !slices.Equal(seed.SSHKeys, want) {
		t.Fatalf("keys = %q", seed.SSHKeys)
	}
}

func TestContextFile(t *testing.T) {
	if n := len(contextFile); n == 0 || n > 8192 {
		t.Fatalf("AGENTS.md is %d bytes; want 1..8192", n)
	}
	lower := strings.ToLower(contextFile)
	for _, s := range []string{"credential.helper store", "unset ssl_cert_file", "https_proxy=", "ubuntu@", "systemctl"} {
		if strings.Contains(lower, s) {
			t.Errorf("AGENTS.md contains %q", s)
		}
	}
}

func TestSeedBootWithoutBootFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSeedISO(dir, map[string][]byte{"other": []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if got, err := SeedBoot(dir); err != nil || got == nil || len(got.SSHPublicKeys) != 0 || len(got.Env) != 0 {
		t.Fatalf("SeedBoot of a seed without boot files = %+v, %v", got, err)
	}
}

func TestWriteSeedISOFailureKeepsOldSeed(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, SeedISO)
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := WriteSeedISO(dir, map[string][]byte{"user-data": []byte("x")}); err == nil {
		t.Fatal("expected error writing into a read-only dir")
	}
	if b, err := os.ReadFile(old); err != nil || string(b) != "old" {
		t.Fatalf("seed.iso = %q, %v; want old seed kept", b, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
