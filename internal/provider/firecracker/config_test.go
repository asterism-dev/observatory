package firecracker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

func TestVMConfig(t *testing.T) {
	f := node.VMFile{MAC: "06:00:c0:a8:40:05", Spec: proto.VMSpec{ID: "vm1", CPUs: 2, MemoryMB: 2048,
		NICs:        []proto.NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:0a", Port: "vm1-mgmt"}},
		Attachments: []proto.DiskAttachment{{DiskID: "d1", URL: "nbd://h:1/d1.img"}}}}
	b, err := vmConfig(f, "/k/vmlinux", "/k/initrd.img", []string{"/v/root.img", "/dev/nbd0", "/v/seed.iso"})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Boot struct {
			Kernel string `json:"kernel_image_path"`
			Initrd string `json:"initrd_path"`
			Args   string `json:"boot_args"`
		} `json:"boot-source"`
		Drives []struct {
			ID       string `json:"drive_id"`
			Path     string `json:"path_on_host"`
			Root     bool   `json:"is_root_device"`
			ReadOnly bool   `json:"is_read_only"`
			Cache    string `json:"cache_type"`
		} `json:"drives"`
		Machine struct {
			VCPUs int `json:"vcpu_count"`
			Mem   int `json:"mem_size_mib"`
		} `json:"machine-config"`
		NICs []struct {
			ID  string `json:"iface_id"`
			MAC string `json:"guest_mac"`
			Dev string `json:"host_dev_name"`
		} `json:"network-interfaces"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Boot.Kernel != "/k/vmlinux" || c.Boot.Initrd != "/k/initrd.img" || !strings.Contains(c.Boot.Args, "console=ttyS0") || !strings.Contains(c.Boot.Args, "root=LABEL=cloudimg-rootfs") {
		t.Errorf("boot = %+v", c.Boot)
	}
	if c.Machine.VCPUs != 2 || c.Machine.Mem != 2048 {
		t.Errorf("machine = %+v", c.Machine)
	}
	if len(c.Drives) != 3 || c.Drives[0].ID != "root" || c.Drives[1].ID != "d1" || c.Drives[1].Path != "/dev/nbd0" || c.Drives[2].ID != "seed" || !c.Drives[2].ReadOnly || c.Drives[0].ReadOnly || c.Drives[0].Root {
		t.Errorf("drives = %+v", c.Drives)
	}
	for _, d := range c.Drives {
		if d.Cache != "Writeback" {
			t.Errorf("drive %s cache_type %q, guest flushes would not reach the host", d.ID, d.Cache)
		}
	}
	if len(c.NICs) != 2 || c.NICs[0].MAC != f.MAC || c.NICs[0].Dev != linuxvm.TapName("vm1", 0) || c.NICs[1].MAC != "02:0a:c8:00:00:0a" || c.NICs[1].Dev != linuxvm.TapName("vm1", 1) {
		t.Errorf("nics = %+v", c.NICs)
	}
	if _, err := vmConfig(f, "k", "i", []string{"only-one"}); err == nil {
		t.Error("accepted a drive list that does not match the attach order")
	}
}

func TestVMConfigBalloonVsock(t *testing.T) {
	b, err := vmConfig(testVMFile(), "k", "i", []string{"/root.img", "/seed.iso"})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Balloon *struct {
			Amount      int  `json:"amount_mib"`
			DeflateOOM  bool `json:"deflate_on_oom"`
			StatsPeriod int  `json:"stats_polling_interval_s"`
		} `json:"balloon"`
		Vsock *struct {
			CID  int    `json:"guest_cid"`
			Path string `json:"uds_path"`
		} `json:"vsock"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Balloon == nil || c.Balloon.Amount != 0 || !c.Balloon.DeflateOOM || c.Balloon.StatsPeriod != 5 {
		t.Errorf("balloon = %+v", c.Balloon)
	}
	if c.Vsock == nil || c.Vsock.CID != 3 || c.Vsock.Path != "/v.sock" {
		t.Errorf("vsock = %+v", c.Vsock)
	}
}

func testVMFile() node.VMFile {
	return node.VMFile{MAC: "06:00:c0:a8:40:05", Spec: proto.VMSpec{ID: "vm1", CPUs: 1, MemoryMB: 512}}
}
