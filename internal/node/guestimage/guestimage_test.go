package guestimage

import (
	_ "embed"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/netaddr"
)

//go:embed docker-daemon.json
var dockerDaemon []byte

const goodSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func art(name string) Artifact {
	return Artifact{Name: name, Version: "1.2.3", URL: "https://example.com/" + name + "/1.2.3/" + name + ".tgz", SHA256: goodSHA}
}

func validManifest() Manifest {
	repo := func(name string) Repo {
		return Repo{
			Name: name, URL: "https://example.com/" + name, Suite: "stable", Component: "main",
			KeyURL: "https://example.com/" + name + ".gpg", Fingerprint: strings.Repeat("AB12", 10),
			Packages: map[string]string{name: "1.0-1"},
		}
	}
	return Manifest{
		Release:     "2026.11.1",
		AptSnapshot: "20261118T000000Z",
		Base:        art("base"),
		Apt:         []string{"git"},
		Repos:       []Repo{repo("docker"), repo("xpra")},
		Files:       []Artifact{art("node"), art("claude"), art("codex"), art("uv"), art("chrome"), art("gh"), art("check-config")},
	}
}

func TestManifestValidate(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	cases := []struct {
		name, want string
		mut        func(*Manifest)
	}{
		{"http url", "files[0].url", func(m *Manifest) { m.Files[0].URL = "http://example.com/node.tgz" }},
		{"short sha", "files[1].sha256", func(m *Manifest) { m.Files[1].SHA256 = goodSHA[:63] }},
		{"uppercase sha", "base.sha256", func(m *Manifest) { m.Base.SHA256 = strings.ToUpper(goodSHA) }},
		{"missing sha", "files[0].sha256", func(m *Manifest) { m.Files[0].SHA256 = "" }},
		{"latest in url", "files[2].url", func(m *Manifest) { m.Files[2].URL = "https://example.com/latest/codex.tgz" }},
		{"latest version", "files[3].version", func(m *Manifest) { m.Files[3].Version = "latest" }},
		{"missing version", "base.version", func(m *Manifest) { m.Base.Version = "" }},
		{"duplicate name", "duplicate", func(m *Manifest) { m.Files[1].Name = "node" }},
		{"missing chrome", "chrome", func(m *Manifest) { m.Files = m.Files[:4] }},
		{"fingerprint spaces", "repos[0].fingerprint", func(m *Manifest) { m.Repos[0].Fingerprint = strings.Repeat("AB12 ", 8) }},
		{"http key url", "repos[1].keyURL", func(m *Manifest) { m.Repos[1].KeyURL = "http://example.com/k.gpg" }},
		{"http repo url", "repos[1].url", func(m *Manifest) { m.Repos[1].URL = "http://example.com/r" }},
		{"no packages", "repos[1].packages", func(m *Manifest) { m.Repos[1].Packages = nil }},
		{"wildcard version", "repos[1].packages", func(m *Manifest) { m.Repos[1].Packages = map[string]string{"xpra": "*"} }},
		{"latest package", "repos[1].packages", func(m *Manifest) { m.Repos[1].Packages = map[string]string{"xpra": "latest"} }},
		{"missing repo", "xpra", func(m *Manifest) { m.Repos = m.Repos[:1] }},
		{"gh as repo", "unexpected \"gh\"", func(m *Manifest) { m.Repos = append(m.Repos, m.Repos[0]); m.Repos[2].Name = "gh" }},
		{"bad snapshot", "aptSnapshot", func(m *Manifest) { m.AptSnapshot = "2026-11-18" }},
		{"bad release", "release", func(m *Manifest) { m.Release = "latest" }},
		{"empty suite", "repos[0].suite", func(m *Manifest) { m.Repos[0].Suite = "" }},
		{"empty component", "repos[1].component", func(m *Manifest) { m.Repos[1].Component = "" }},
		{"empty apt entry", "apt[1]", func(m *Manifest) { m.Apt = []string{"git", ""} }},
	}
	for _, c := range cases {
		m := validManifest()
		c.mut(&m)
		err := m.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}
}

func TestParseManifestStrict(t *testing.T) {
	b, _ := json.Marshal(validManifest())
	if _, err := ParseManifest(b); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest([]byte(strings.Replace(string(b), `{`, `{"extra":1,`, 1))); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func imageFiles(t *testing.T, m Manifest) map[string][]byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		"manifest.json":                   b,
		"vmm-image-build.sh":              []byte("#!/bin/sh\n"),
		"docker-daemon.json":              dockerDaemon,
		"agent-browser-package.json":      []byte("{}"),
		"agent-browser-package-lock.json": []byte("{\"lock\":1}"),
	}
}

func TestImageVersionChanges(t *testing.T) {
	base, err := NewImage(imageFiles(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^2026\.11\.1-[0-9a-f]{8}$`).MatchString(base.Version) {
		t.Fatalf("version %q", base.Version)
	}
	again, _ := NewImage(imageFiles(t, validManifest()))
	if again.Version != base.Version {
		t.Fatalf("same files, versions %q != %q", again.Version, base.Version)
	}
	variants := map[string]func(map[string][]byte, *Manifest){
		"manifest apt": func(_ map[string][]byte, m *Manifest) { m.Apt = []string{"git", "jq"} },
		"build script": func(f map[string][]byte, _ *Manifest) { f["vmm-image-build.sh"] = []byte("#!/bin/sh\n:") },
		"lockfile":     func(f map[string][]byte, _ *Manifest) { f["agent-browser-package-lock.json"] = []byte("{\"lock\":2}") },
		"base sha":     func(_ map[string][]byte, m *Manifest) { m.Base.SHA256 = strings.Repeat("f", 64) },
	}
	for name, mut := range variants {
		m := validManifest()
		f := imageFiles(t, m)
		mut(f, &m)
		if name == "manifest apt" || name == "base sha" {
			f["manifest.json"], _ = json.Marshal(m)
		}
		img, err := NewImage(f)
		if err != nil {
			t.Fatal(name, err)
		}
		if img.Version == base.Version {
			t.Errorf("%s: version did not change", name)
		}
	}
}

func TestDockerPoolsDisjoint(t *testing.T) {
	var cfg struct {
		Bip   string `json:"bip"`
		Pools []struct {
			Base string `json:"base"`
			Size int    `json:"size"`
		} `json:"default-address-pools"`
	}
	if err := json.Unmarshal(dockerDaemon, &cfg); err != nil {
		t.Fatal(err)
	}
	forbidden := []netip.Prefix{
		netaddr.MgmtPool, netaddr.DevicePool, netaddr.UnderlayPool, netaddr.Transit,
		netaddr.AutoNetworks, netaddr.NATNet, netip.MustParsePrefix("100.64.0.0/10"),
	}
	check := func(label, s string) netip.Prefix {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		p = p.Masked()
		for _, f := range forbidden {
			if f.Overlaps(p) {
				t.Errorf("%s %s overlaps %s", label, s, f)
			}
		}
		if netip.MustParsePrefix("192.168.0.0/16").Overlaps(p) {
			t.Errorf("%s %s inside 192.168.0.0/16", label, s)
		}
		return p
	}
	bip := check("bip", cfg.Bip)
	var covered int
	for _, p := range cfg.Pools {
		pfx := check("pool", p.Base)
		if pfx.Overlaps(bip) {
			t.Errorf("pool %s overlaps bip %s", p.Base, cfg.Bip)
		}
		if p.Size != 24 {
			t.Errorf("pool %s size %d", p.Base, p.Size)
		}
		if !netip.MustParsePrefix("172.16.0.0/12").Contains(pfx.Addr()) || pfx.Addr().As4()[1] < 18 {
			t.Errorf("pool %s outside 172.18-172.31", p.Base)
			continue
		}
		covered += 1 << (24 - pfx.Bits())
	}
	if covered != 14*256 {
		t.Errorf("pools cover %d /24s, want 172.18-172.31 exactly (%d)", covered, 14*256)
	}
}

func TestCurrentImage(t *testing.T) {
	img, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"manifest.json", "vmm-image-build.sh", "docker-daemon.json", "agent-browser-package.json", "agent-browser-package-lock.json"} {
		if len(img.Files[n]) == 0 {
			t.Errorf("Files[%q] missing", n)
		}
	}
	if len(img.Files) != 5 {
		t.Errorf("Files has %d entries, want 5", len(img.Files))
	}
}

func TestBuildScriptCoversManifest(t *testing.T) {
	img, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	script := string(img.Files["vmm-image-build.sh"])
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^\s*([a-z0-9| -]+)\)`).FindAllStringSubmatch(script, -1) {
		for _, l := range strings.Split(m[1], "|") {
			names = append(names, strings.TrimSpace(l))
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^([a-z_][a-z0-9_]*)\(\)`).FindAllStringSubmatch(script, -1) {
		names = append(names, m[1])
	}
	for _, f := range img.Manifest.Files {
		if !slices.Contains(names, f.Name) {
			t.Errorf("file %q has no case label or function in the build script", f.Name)
		}
	}
	for _, r := range img.Manifest.Repos {
		if !slices.Contains(names, r.Name) {
			t.Errorf("repo %q has no case label or function in the build script", r.Name)
		}
	}
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`latest`),
		regexp.MustCompile(`curl[^\n]*\|\s*(ba|da)?sh`),
		regexp.MustCompile(`apt-get[^\n]*upgrade`),
	} {
		if bad.MatchString(script) {
			t.Errorf("build script matches %s", bad)
		}
	}
}

func TestBuildScriptSyntax(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "vmm-image-build.sh").CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	if out, err := exec.Command(sc, "-s", "sh", "vmm-image-build.sh").CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}

func TestBuildScriptMasksXpraSystemServer(t *testing.T) {
	b, err := os.ReadFile("vmm-image-build.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^systemctl mask xpra-server\.socket xpra-server\.service$`).Match(b) {
		t.Error("build script leaves the xpra system proxy (TCP 14500) enabled")
	}
}

func TestBuildScriptRepoCheckFailsClosed(t *testing.T) {
	b, err := os.ReadFile("vmm-image-build.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\[ -s "\$tmp/offered" \] \|\| fail `).Match(b) {
		t.Error("an empty list of third-party packages passes the repo check")
	}
}
