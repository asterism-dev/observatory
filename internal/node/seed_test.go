package node

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"

	"github.com/asterism-dev/observatory/internal/node/guest"
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

func TestSeedHasEgressCAs(t *testing.T) {
	cas := map[string]string{"0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01": testCA}
	var got map[string]string
	if err := json.Unmarshal(GuestSeedFiles(VMFile{Boot: &proto.Boot{EgressCAs: cas}}, PrimaryNet{})["vmm-egress-cas"], &got); err != nil || !reflect.DeepEqual(got, cas) {
		t.Fatalf("vmm-egress-cas = %v, %v; want %v", got, err, cas)
	}
	if got := string(GuestSeedFiles(VMFile{}, PrimaryNet{})["vmm-egress-cas"]); got != "{}" {
		t.Fatalf("vmm-egress-cas without a boot = %q", got)
	}
}

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

func TestSeedHasAgent(t *testing.T) {
	if got := GuestSeedFiles(VMFile{}, PrimaryNet{})["vmm-agent.py"]; string(got) != guest.AgentScript || len(got) == 0 {
		t.Fatalf("seed vmm-agent.py = %q", got)
	}
}

func TestSeedHasIntegrationsScript(t *testing.T) {
	if got := GuestSeedFiles(VMFile{}, PrimaryNet{})["vmm-integrations.py"]; string(got) != guest.IntegrationsScript || len(got) == 0 {
		t.Fatalf("seed vmm-integrations.py = %q", got)
	}
}

func TestSeedHasContextFile(t *testing.T) {
	if got := GuestSeedFiles(VMFile{}, PrimaryNet{})["vmm-AGENTS.md"]; string(got) != guest.ContextFile || len(got) == 0 {
		t.Fatalf("seed vmm-AGENTS.md = %q", got)
	}
}

func TestSeedHasDesktopScript(t *testing.T) {
	if got := GuestSeedFiles(VMFile{}, PrimaryNet{})["vmm-desktop"]; string(got) != guest.DesktopScript || len(got) == 0 {
		t.Fatalf("seed vmm-desktop = %q", got)
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

var trustBlock = []string{
	`CURL_CA_BUNDLE="/etc/ssl/certs/ca-certificates.crt"`, `NODE_EXTRA_CA_CERTS="/etc/ssl/certs/ca-certificates.crt"`,
	`REQUESTS_CA_BUNDLE="/etc/ssl/certs/ca-certificates.crt"`, `SSL_CERT_FILE="/etc/ssl/certs/ca-certificates.crt"`,
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
	if got, want := vmmBlock(t, environment), append([]string{`B="` + env["B"] + `"`, `C=""`}, trustBlock...); !slices.Equal(got, want) {
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
	if environment != "PATH=\"/usr/bin\"\n# BEGIN vmm\n"+strings.Join(trustBlock, "\n")+"\n# END vmm\n" {
		t.Fatalf("/etc/environment after second boot:\n%s", environment)
	}
}

func TestBootScriptTrust(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	id := "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01"
	runBoot(t, root, GuestSeedFiles(VMFile{Boot: &proto.Boot{EgressCAs: map[string]string{id: testCA}}}, PrimaryNet{}))
	if got := readFile(t, filepath.Join(root, "usr/local/share/ca-certificates/observatory-egress-"+id+".crt")); got != testCA {
		t.Fatalf("ca file = %q", got)
	}
	bundle := "/etc/ssl/certs/ca-certificates.crt"
	names := []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}
	if got := readFile(t, filepath.Join(root, "etc/vmm/env.sh")); !strings.Contains(got, "export SSL_CERT_FILE='"+bundle+"'\n") {
		t.Fatalf("env.sh = %q", got)
	}
	if got := vmmBlock(t, readFile(t, filepath.Join(root, "etc/environment"))); !slices.Contains(got, `NODE_EXTRA_CA_CERTS="`+bundle+`"`) {
		t.Fatalf("managed block = %q", got)
	}
	for _, d := range []string{"system", "user"} {
		got := readFile(t, filepath.Join(root, "etc/systemd", d+".conf.d", "50-vmm-trust.conf"))
		if !strings.HasPrefix(got, "[Manager]\n") || !strings.Contains(got, "DefaultEnvironment=") {
			t.Fatalf("%s.conf.d = %q", d, got)
		}
		for _, n := range names {
			if !strings.Contains(got, n+"="+bundle) {
				t.Fatalf("%s.conf.d misses %s: %q", d, n, got)
			}
		}
	}

	runBoot(t, root, GuestSeedFiles(VMFile{Boot: &proto.Boot{Env: map[string]string{"SSL_CERT_FILE": "/x"}}}, PrimaryNet{}))
	if got := readFile(t, filepath.Join(root, "etc/vmm/env.sh")); strings.Count(got, "SSL_CERT_FILE=") != 1 || !strings.Contains(got, "export SSL_CERT_FILE='/x'\n") {
		t.Fatalf("env.sh with a user SSL_CERT_FILE = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/local/share/ca-certificates/observatory-egress-"+id+".crt")); !os.IsNotExist(err) {
		t.Fatalf("ca file kept without cas in the seed: %v", err)
	}
}

func TestBootScriptInstallsContext(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	path := filepath.Join(root, "usr/local/share/vmm/AGENTS.md")
	link := filepath.Join(root, "etc/claude-code/CLAUDE.md")
	for range 2 {
		runBoot(t, root, GuestSeedFiles(VMFile{}, PrimaryNet{}))
		if got := readFile(t, path); got != guest.ContextFile {
			t.Fatalf("AGENTS.md = %q", got)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("AGENTS.md: %v, %v", st, err)
		}
		if got, err := os.Readlink(link); err != nil || got != "/usr/local/share/vmm/AGENTS.md" {
			t.Fatalf("CLAUDE.md link = %q, %v", got, err)
		}
	}
}

func TestBootScriptKeepsOwnClaudeMemory(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	link := filepath.Join(root, "etc/claude-code/CLAUDE.md")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.WriteFile(link, []byte("org policy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runBoot(t, root, GuestSeedFiles(VMFile{}, PrimaryNet{}))
	if st, err := os.Lstat(link); err != nil || !st.Mode().IsRegular() || readFile(t, link) != "org policy\n" {
		t.Fatalf("own CLAUDE.md replaced: %v, %v", st, err)
	}
}

func TestBootScriptInstallsIntegrations(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	tok := "ghp_dummy0000000000000000"
	f := VMFile{Integrations: []proto.IntegrationToken{{Kind: "github", Token: tok}}}
	runBoot(t, root, GuestSeedFiles(f, PrimaryNet{}))
	for path, mode := range map[string]os.FileMode{
		"usr/local/lib/vmm/vmm-integrations.py":       0o644,
		"usr/local/bin/vmm-token":                     0o755,
		"usr/local/bin/git-credential-vmm":            0o755,
		"usr/local/bin/gh":                            0o755,
		"etc/vmm/gitconfig":                           0o644,
		"etc/profile.d/vmm-linear.sh":                 0o644,
		"etc/systemd/system/vmm-integrations.service": 0o644,
		"etc/systemd/system/vmm-integrations.timer":   0o644,
	} {
		st, err := os.Stat(filepath.Join(root, path))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want %v", path, st, err, mode)
		}
	}
	if got := readFile(t, filepath.Join(root, "usr/local/lib/vmm/vmm-integrations.py")); got != guest.IntegrationsScript {
		t.Error("installed script differs from the seed's")
	}
	if got := readFile(t, filepath.Join(root, "usr/local/bin/vmm-token")); !strings.Contains(got, "exec python3 /usr/local/lib/vmm/vmm-integrations.py token \"$@\"") {
		t.Errorf("vmm-token shim = %q", got)
	}
	timer := readFile(t, filepath.Join(root, "etc/systemd/system/vmm-integrations.timer"))
	for _, want := range []string{"OnBootSec=30s\n", "OnUnitActiveSec=5min\n", "WantedBy=timers.target\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer misses %q: %q", want, timer)
		}
	}
	if got := readFile(t, filepath.Join(root, "etc/systemd/system/vmm-integrations.service")); !strings.Contains(got, "Type=oneshot\n") || !strings.Contains(got, "ExecStart=/usr/bin/python3 /usr/local/lib/vmm/vmm-integrations.py sync\n") {
		t.Errorf("service = %q", got)
	}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), tok) {
				t.Errorf("%s contains the token", p)
			}
		}
		return nil
	})
}

func TestBootScriptInstallsDesktop(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	runBoot(t, root, GuestSeedFiles(VMFile{}, PrimaryNet{}))
	for path, mode := range map[string]os.FileMode{
		"usr/local/bin/vmm-desktop":                 0o755,
		"etc/systemd/user/vmm-desktop.service":      0o644,
		"etc/systemd/user/vmm-desktop-idle.service": 0o644,
		"etc/systemd/user/vmm-desktop-idle.timer":   0o644,
		"var/lib/systemd/linger/ubuntu":             0o644,
	} {
		st, err := os.Stat(filepath.Join(root, path))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want %v", path, st, err, mode)
		}
	}
	if got := readFile(t, filepath.Join(root, "usr/local/bin/vmm-desktop")); got != guest.DesktopScript {
		t.Error("installed vmm-desktop differs from the seed's")
	}
	var cmdline string
	for l := range strings.Lines(readFile(t, filepath.Join(root, "etc/systemd/user/vmm-desktop.service"))) {
		if strings.HasPrefix(l, "ExecStart=") {
			cmdline = l
		}
	}
	for _, want := range []string{"ExecStart=/usr/bin/xpra start :100 ", "--daemon=no", "--systemd-run=no", "--socket-dir=%t/xpra", "-nolisten tcp", "--html=off", "--mdns=no"} {
		if !strings.Contains(cmdline, want) {
			t.Errorf("ExecStart misses %q: %q", want, cmdline)
		}
	}
	// xpra 6.5 rejects an empty --bind-tcp=, and auto binds also listen on an abstract socket any local UID can reach.
	for _, bad := range []string{"--bind-tcp", "--bind-ws", "--bind-ssl", "--bind-vsock", "--bind=auto"} {
		if strings.Contains(cmdline, bad) {
			t.Errorf("ExecStart has %q: %q", bad, cmdline)
		}
	}
	timer := readFile(t, filepath.Join(root, "etc/systemd/user/vmm-desktop-idle.timer"))
	for _, want := range []string{"PartOf=vmm-desktop.service\n", "OnActiveSec=1min\n", "OnUnitActiveSec=1min\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("idle timer misses %q: %q", want, timer)
		}
	}
	if got := readFile(t, filepath.Join(root, "etc/systemd/user/vmm-desktop-idle.service")); !strings.Contains(got, "ExecStart=/usr/local/bin/vmm-desktop idle-check\n") {
		t.Errorf("idle service = %q", got)
	}
}
