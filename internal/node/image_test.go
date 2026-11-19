package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func writeBase(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("base"), size/4)
	p := filepath.Join(t.TempDir(), "base.raw")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func TestCloneResize(t *testing.T) {
	base, data := writeBase(t, 1<<20)
	dst := filepath.Join(t.TempDir(), "root.img")
	if err := CloneResize(base, dst, 4); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4<<30 {
		t.Fatalf("size = %d", st.Size())
	}
	f, _ := os.Open(dst)
	defer f.Close()
	head := make([]byte, len(data))
	f.ReadAt(head, 0)
	if !bytes.Equal(head, data) {
		t.Fatal("content differs from base")
	}
}

func TestCloneResizeTooSmall(t *testing.T) {
	base, _ := writeBase(t, 1<<20)
	f, _ := os.OpenFile(base, os.O_RDWR, 0)
	f.Truncate(2 << 30)
	f.Close()
	if err := CloneResize(base, filepath.Join(t.TempDir(), "root.img"), 1); err == nil {
		t.Fatal("expected error for disk smaller than base")
	}
}

func TestCloudInit(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{ID: "abc123", Name: "dev"}, MAC: "7e:30:17:4e:4a:00"}
	ud, md, nc := CloudInit(f, PrimaryNet{})
	for _, want := range []string{"#cloud-config\n", "hostname: dev", "name: ubuntu", "NOPASSWD:ALL", "console=hvc0", "update-grub"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data missing %q:\n%s", want, ud)
		}
	}
	if !strings.Contains(md, "instance-id: abc123") || !strings.Contains(md, "local-hostname: dev") {
		t.Errorf("meta-data = %q", md)
	}
	// Ubuntu defaults to a DUID client id, which hides the MAC from macOS' dhcpd_leases.
	for _, want := range []string{"version: 2", `macaddress: "7e:30:17:4e:4a:00"`, "dhcp4: true", "dhcp-identifier: mac"} {
		if !strings.Contains(nc, want) {
			t.Errorf("network-config missing %q:\n%s", want, nc)
		}
	}
}

func TestCloudInitHasNoKeys(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{ID: "abc123", Name: "dev"}, Boot: &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAAAkey me@host"}}}
	ud, _, _ := CloudInit(f, PrimaryNet{})
	if strings.Contains(ud, "ssh_authorized_keys") || strings.Contains(ud, "AAAAkey") {
		t.Fatalf("user-data holds keys:\n%s", ud)
	}
}

func TestEnsureBaseImageDownloadsOnce(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	p1, err := EnsureBaseImage(dir, srv.URL+"/img", "")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := EnsureBaseImage(dir, srv.URL+"/img", "")
	if err != nil || p1 != p2 {
		t.Fatalf("second call: %q %v", p2, err)
	}
	got, _ := os.ReadFile(p1)
	if !bytes.Equal(got, data) || hits.Load() != 1 {
		t.Fatalf("content ok=%v hits=%d", bytes.Equal(got, data), hits.Load())
	}
}

func TestEnsureBaseImageHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	dir := t.TempDir()
	if _, err := EnsureBaseImage(dir, srv.URL+"/img", ""); err == nil {
		t.Fatal("expected error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestEnsureBaseImageStalledServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	old := downloadClient
	t.Cleanup(func() { downloadClient = old })
	c := *downloadClient
	c.Timeout = 200 * time.Millisecond
	downloadClient = &c
	dir := t.TempDir()
	if _, err := EnsureBaseImage(dir, srv.URL+"/img", ""); err == nil {
		t.Fatal("expected a timeout")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestCloudInitMgmtNIC(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{Name: "dev", NICs: []proto.NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:14", IP: "10.200.0.20", Port: "x-mgmt"}}}, MAC: "7e:30:17:4e:4a:00"}
	_, _, nc := CloudInit(f, PrimaryNet{})
	for _, want := range []string{"mgmt0:", `macaddress: "02:0a:c8:00:00:14"`, "dhcp4: true", "use-dns: false", "use-hostname: false", "use-domains: false"} {
		if !strings.Contains(nc, want) {
			t.Errorf("network-config missing %q:\n%s", want, nc)
		}
	}
	if strings.Contains(nc, "10.200.0.20") || strings.Contains(nc, "mtu") {
		t.Errorf("mgmt nic must not carry static config:\n%s", nc)
	}
	mgmt := nc[strings.Index(nc, "mgmt0:"):]
	primary := nc[:strings.Index(nc, "mgmt0:")]
	for _, want := range []string{"link-local: []", "accept-ra: false"} {
		if !strings.Contains(mgmt, want) {
			t.Errorf("mgmt block missing %q:\n%s", want, nc)
		}
		if strings.Contains(primary, want) {
			t.Errorf("primary block must not contain %q:\n%s", want, nc)
		}
	}
}

func TestCloudInitMgmtDNS(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{Name: "dev", NICs: []proto.NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:14"}}}, MAC: "7e:30:17:4e:4a:00"}
	_, _, nc := CloudInit(f, PrimaryNet{})
	for _, want := range []string{"addresses: [10.192.0.1]", `search: ["~internal"]`} {
		if !strings.Contains(nc, want) {
			t.Errorf("network-config missing %q:\n%s", want, nc)
		}
	}
	if strings.Contains(nc, "10.201.0.1") {
		t.Errorf("network-config still uses 10.201.0.1:\n%s", nc)
	}
}

func TestCloudInitNetworkNICs(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{Name: "dev", NICs: []proto.NIC{
		{Network: "mgmt", MAC: "02:0a:c8:00:00:14"},
		{Network: "n1", MAC: "02:00:00:00:01:01"},
		{Network: "n2", MAC: "02:00:00:00:02:01"},
	}}, MAC: "7e:30:17:4e:4a:00"}
	ud, _, nc := CloudInit(f, PrimaryNet{})
	if !strings.Contains(ud, "when: [boot]") {
		t.Errorf("user-data missing network update on boot:\n%s", ud)
	}
	mgmtAt, net0At, net1At := strings.Index(nc, "  mgmt0:"), strings.Index(nc, "  net0:"), strings.Index(nc, "  net1:")
	if mgmtAt < 0 || net0At < mgmtAt || net1At < net0At {
		t.Fatalf("network-config entries missing or out of order:\n%s", nc)
	}
	mgmt, net0, net1 := nc[mgmtAt:net0At], nc[net0At:net1At], nc[net1At:]
	for _, want := range []string{`macaddress: "02:0a:c8:00:00:14"`, "nameservers:", "10.192.0.1", `"~internal"`} {
		if !strings.Contains(mgmt, want) {
			t.Errorf("mgmt0 missing %q:\n%s", want, nc)
		}
	}
	for mac, block := range map[string]string{"02:00:00:00:01:01": net0, "02:00:00:00:02:01": net1} {
		for _, want := range []string{`macaddress: "` + mac + `"`, "dhcp4: true", "link-local: []", "accept-ra: false", "use-dns: false", "use-hostname: false", "use-domains: false"} {
			if !strings.Contains(block, want) {
				t.Errorf("net block for %s missing %q:\n%s", mac, want, nc)
			}
		}
		if strings.Contains(block, "nameservers") {
			t.Errorf("net block for %s has nameservers:\n%s", mac, nc)
		}
	}
}

func TestCloudInitRunsDiskScriptEveryBoot(t *testing.T) {
	ud, _, _ := CloudInit(VMFile{Spec: proto.VMSpec{Name: "dev"}}, PrimaryNet{})
	boot := ud[strings.Index(ud, "bootcmd:"):strings.Index(ud, "updates:")]
	if !strings.Contains(boot, "LABEL=cidata") || !strings.Contains(boot, "sh /run/vmm-seed/vmm-disks.sh") {
		t.Fatalf("bootcmd does not run vmm-disks.sh:\n%s", ud)
	}
	// User-data is only re-read with a new instance-id, so the boot script runs through vmm-disks.sh instead.
	if strings.Contains(boot, "vmm-boot.sh") {
		t.Fatalf("bootcmd runs vmm-boot.sh itself:\n%s", boot)
	}
}

func TestDiskScriptRunsBootScriptBeforeReady(t *testing.T) {
	s := DiskScript(VMFile{MAC: "52:54:00:12:34:56", Spec: proto.VMSpec{Attachments: []proto.DiskAttachment{{DiskID: "d1", MountPath: "/data"}}}})
	i := strings.Index(s, "sh /run/vmm-seed/vmm-boot.sh /run/vmm-seed\n")
	if i < 0 || i > strings.Index(s, "systemd-run") || i > strings.Index(s, "mkfs") {
		t.Fatalf("script does not run vmm-boot.sh before the disks and the ready probe:\n%s", s)
	}
}

func TestDiskScript(t *testing.T) {
	s := DiskScript(VMFile{Spec: proto.VMSpec{Attachments: []proto.DiskAttachment{{DiskID: "d1", MountPath: "/data"}, {DiskID: "d2", MountPath: "/it's"}}}})
	for _, want := range []string{"dev='/dev/vdb'", "dev='/dev/vdc'", `blkid -p "$dev" >/dev/null 2>&1; rc=$?`,
		`if [ $rc -eq 2 ] && dd if="$dev" of=/run/vmm-probe bs=1M count=1 iflag=direct,fullblock 2>/dev/null && [ "$(wc -c </run/vmm-probe)" -eq 1048576 ] && [ "$(tr -d '\000' </run/vmm-probe | wc -c)" -eq 0 ]; then`,
		`mkfs.ext4 -q -E nodiscard "$dev" && new=1`, "mountpoint -q '/data' || mount \"$dev\" '/data'; } && {", "'/data'", `'/it'\''s'`, "resize2fs", "chown 1000:1000 '/data'"} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "chown ubuntu") {
		t.Errorf("chown by name fails on first boot, before cloud-init creates the user:\n%s", s)
	}
	if strings.Contains(s, "|| { mkfs") {
		t.Errorf("formats on any blkid failure:\n%s", s)
	}
	if s := DiskScript(VMFile{}); strings.Contains(s, "mkfs") {
		t.Fatalf("empty script formats:\n%s", s)
	}
}

func TestDiskScriptEmitsReadyMarker(t *testing.T) {
	for _, f := range []VMFile{
		{MAC: "52:54:00:12:34:56"},
		{MAC: "52:54:00:12:34:56", Spec: proto.VMSpec{Attachments: []proto.DiskAttachment{{DiskID: "d1", MountPath: "/data"}}}},
	} {
		s := DiskScript(f)
		for _, want := range []string{"systemd-run", "--description=", "52:54:00:12:34:56", "/dev/ttyS0", "/dev/hvc0"} {
			if !strings.Contains(s, want) {
				t.Errorf("script missing %q:\n%s", want, s)
			}
		}
		// systemd expands $VAR and %-specifiers in ExecStart, which would blank the probe's shell variables.
		probe := s[strings.Index(s, "systemd-run"):]
		// systemd < 254 has no --expand-environment, so the probe runs from a script file systemd never expands.
		if strings.Contains(probe, "--expand-environment") || !strings.Contains(probe, "sh /run/vmm-ready.sh") || !strings.Contains(s, "<<'VMM_READY'") {
			t.Errorf("probe depends on systemd expansion rules:\n%s", s)
		}
		if n := strings.Count(s, ReadyMarker); n != 1 {
			t.Errorf("marker appears %d times, want 1:\n%s", n, s)
		}
	}
}

func TestCloudInitOVNNICsAreOptional(t *testing.T) {
	f := VMFile{MAC: "52:54:00:00:00:01", Spec: proto.VMSpec{NICs: []proto.NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:0a"}, {Network: "net1", MAC: "02:0a:d2:00:00:0a"}}}}
	_, _, nc := CloudInit(f, PrimaryNet{})
	// Without it, wait-online holds cloud-init (and the ready probe) for 2 minutes whenever OVN is down.
	if n := strings.Count(nc, "optional: true"); n != 2 {
		t.Errorf("optional count = %d, want 2 (OVN NICs only):\n%s", n, nc)
	}
	primary := nc[:strings.Index(nc, "mgmt0:")]
	if strings.Contains(primary, "optional") {
		t.Errorf("primary NIC must stay required:\n%s", nc)
	}
}

func TestCloudInitStaticPrimary(t *testing.T) {
	f := VMFile{MAC: "06:00:c0:a8:40:05"}
	_, _, nc := CloudInit(f, PrimaryNet{Address: "192.168.64.5/24", Gateway: "192.168.64.1"})
	for _, want := range []string{`macaddress: "06:00:c0:a8:40:05"`, `addresses: ["192.168.64.5/24"]`, "to: default", "via: 192.168.64.1", "addresses: [1.1.1.1, 9.9.9.9]"} {
		if !strings.Contains(nc, want) {
			t.Errorf("network-config lacks %q:\n%s", want, nc)
		}
	}
	if strings.Contains(nc, "dhcp4: true") || strings.Contains(nc, "dhcp-identifier") {
		t.Errorf("static primary must not use DHCP:\n%s", nc)
	}
	_, _, dhcp := CloudInit(f, PrimaryNet{})
	if !strings.Contains(dhcp, "dhcp4: true") || !strings.Contains(dhcp, "dhcp-identifier: mac") {
		t.Errorf("zero PrimaryNet must keep DHCP:\n%s", dhcp)
	}
}

func TestEnsureBaseImageChecksum(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(data) }))
	defer srv.Close()
	sum := sha256.Sum256(data)

	dir := t.TempDir()
	if _, err := EnsureBaseImage(dir, srv.URL+"/img", hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("right hash: %v", err)
	}

	bad := t.TempDir()
	_, err := EnsureBaseImage(bad, srv.URL+"/img", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("wrong hash: %v", err)
	}
	if entries, _ := os.ReadDir(bad); len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}

	if _, err := EnsureBaseImage(t.TempDir(), srv.URL+"/img", ""); err != nil {
		t.Fatalf("empty hash: %v", err)
	}
}

func TestCloudInitDockerGroup(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{ID: "abc123", Name: "dev"}, Image: "2026.11.1-aaaaaaaa"}
	if ud, _, _ := CloudInit(f, PrimaryNet{}); !strings.Contains(ud, "    shell: /bin/bash\n    groups: [docker]\n") {
		t.Errorf("user-data without docker group:\n%s", ud)
	}
	f.Image = ""
	if ud, _, _ := CloudInit(f, PrimaryNet{}); strings.Contains(ud, "docker") {
		t.Errorf("stock image user-data has docker group:\n%s", ud)
	}
}
