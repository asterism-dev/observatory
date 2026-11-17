package node

import (
	"io"
	"os"
	"os/exec"
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

func TestSeedKeysTrimmed(t *testing.T) {
	files := GuestSeedFiles(VMFile{Boot: &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAAA one@host\n", "", "  \n", "ssh-rsa BB\nssh-rsa CC", " ssh-rsa DD two\r\n"}}}, PrimaryNet{})
	if got := string(files["vmm-authorized-keys"]); got != "ssh-ed25519 AAAA one@host\nssh-rsa DD two\n" {
		t.Fatalf("vmm-authorized-keys = %q", got)
	}
}

func TestSeedBootWithoutBootFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSeedISO(dir, map[string][]byte{"user-data": []byte("x")}); err != nil {
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

// runBoot runs the seed's vmm-boot.sh against root as the guest would.
func runBoot(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	seed := t.TempDir()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(seed, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", filepath.Join(seed, "vmm-boot.sh"), seed)
	cmd.Env = append(os.Environ(), "VMM_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) > 0 {
		t.Fatalf("vmm-boot.sh: %v: %s", err, out)
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

// sourcedEnv sources env.sh and reports A to E as a child process sees them.
func sourcedEnv(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", `. "$1"; exec sh -c 'printf "%s\0" "${A-unset}" "${B-unset}" "${C-unset}" "${D-unset}" "${E-unset}"'`, "sh", filepath.Join(root, "etc/vmm/env.sh")).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func vmmBlock(t *testing.T, environment string) []string {
	t.Helper()
	i, j := strings.Index(environment, "# BEGIN vmm\n"), strings.Index(environment, "# END vmm\n")
	if i < 0 && j < 0 {
		return nil
	}
	if i < 0 || j < i {
		t.Fatalf("broken managed block:\n%s", environment)
	}
	return strings.Split(strings.TrimSuffix(environment[i+len("# BEGIN vmm\n"):j], "\n"), "\n")
}

func TestBootFilesRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/environment"), []byte("PATH=\"/usr/bin\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := []string{"ssh-ed25519 AAAA one@host", "ssh-rsa BBBB two@host"}
	// pam_env cuts D at the # and splits E at its line buffer, so both stay out of /etc/environment.
	env := map[string]string{"A": "x\ny", "B": `it's $HOME \ "q" =`, "C": "", "D": "x#y", "E": strings.Repeat("a", 1001) + "X=injected"}
	files := GuestSeedFiles(VMFile{Boot: &proto.Boot{SSHPublicKeys: keys, Env: env}}, PrimaryNet{})
	for _, name := range []string{"vmm-boot.sh", "vmm-env", "vmm-authorized-keys"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("seed misses %s", name)
		}
	}
	runBoot(t, root, files)

	if got := readFile(t, filepath.Join(root, "etc/vmm/authorized_keys")); got != keys[0]+"\n"+keys[1]+"\n" {
		t.Fatalf("authorized_keys = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "etc/ssh/sshd_config.d/50-vmm.conf")); got != "Match User ubuntu\n\tAuthorizedKeysFile .ssh/authorized_keys /etc/vmm/authorized_keys\n" {
		t.Fatalf("50-vmm.conf = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "etc/profile.d/vmm-env.sh")); !strings.Contains(got, "/etc/vmm/env.sh") {
		t.Fatalf("profile.d/vmm-env.sh = %q", got)
	}
	if got, want := sourcedEnv(t, root), "x\ny\x00"+env["B"]+"\x00\x00x#y\x00"+env["E"]+"\x00"; got != want {
		t.Fatalf("env.sh sets %q, want %q", got, want)
	}
	environment := readFile(t, filepath.Join(root, "etc/environment"))
	if !strings.HasPrefix(environment, "PATH=\"/usr/bin\"\n") {
		t.Fatalf("/etc/environment lost its own lines:\n%s", environment)
	}
	if got, want := vmmBlock(t, environment), []string{`B="` + env["B"] + `"`, `C=""`}; !slices.Equal(got, want) {
		t.Fatalf("managed block = %q, want %q", got, want)
	}
	for _, p := range []string{"etc/vmm/authorized_keys", "etc/vmm/env.sh", "etc/environment"} {
		if st, _ := os.Stat(filepath.Join(root, p)); st.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %v", p, st.Mode())
		}
	}

	runBoot(t, root, GuestSeedFiles(VMFile{Boot: &proto.Boot{SSHPublicKeys: keys[:1]}}, PrimaryNet{}))
	if got := readFile(t, filepath.Join(root, "etc/vmm/authorized_keys")); got != keys[0]+"\n" {
		t.Fatalf("authorized_keys after second boot = %q", got)
	}
	if got := sourcedEnv(t, root); got != strings.Repeat("unset\x00", 5) {
		t.Fatalf("env.sh after second boot sets %q", got)
	}
	environment = readFile(t, filepath.Join(root, "etc/environment"))
	if environment != "PATH=\"/usr/bin\"\n" {
		t.Fatalf("/etc/environment after second boot:\n%s", environment)
	}
}
