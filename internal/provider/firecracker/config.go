package firecracker

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw rdinit=/init"

const (
	guestCID  = 3
	vsockPath = "/v.sock"
)

type fcDrive struct {
	ID       string `json:"drive_id"`
	Path     string `json:"path_on_host"`
	Root     bool   `json:"is_root_device"`
	ReadOnly bool   `json:"is_read_only"`
	// Writeback makes Firecracker fsync on guest flushes; the default "Unsafe" acknowledges them without writing
	Cache string `json:"cache_type"`
}

type fcNIC struct {
	ID  string `json:"iface_id"`
	MAC string `json:"guest_mac"`
	Dev string `json:"host_dev_name"`
}

// vmConfig is Firecracker's --config-file; drives holds the host path of each node.AttachOrder disk.
func vmConfig(f node.VMFile, kernel, initrd string, drives []string) ([]byte, error) {
	order := node.AttachOrder(f)
	if len(drives) != len(order) {
		return nil, fmt.Errorf("got %d drive paths for %d disks", len(drives), len(order))
	}
	var ds []fcDrive
	for i, d := range order {
		ds = append(ds, fcDrive{ID: d.Name, Path: drives[i], ReadOnly: d.ReadOnly, Cache: "Writeback"})
	}
	nics := []fcNIC{{ID: "eth0", MAC: f.MAC, Dev: linuxvm.TapName(f.Spec.ID, 0)}}
	for i, n := range f.Spec.NICs {
		nics = append(nics, fcNIC{ID: "net" + strconv.Itoa(i), MAC: n.MAC, Dev: linuxvm.TapName(f.Spec.ID, i+1)})
	}
	return json.MarshalIndent(map[string]any{
		"boot-source":        map[string]string{"kernel_image_path": kernel, "initrd_path": initrd, "boot_args": bootArgs},
		"drives":             ds,
		"machine-config":     map[string]int{"vcpu_count": f.Spec.CPUs, "mem_size_mib": f.Spec.MemoryMB},
		"network-interfaces": nics,
		// An empty balloon only reports guest memory statistics; deflate_on_oom keeps a future inflate from starving the guest.
		"balloon": map[string]any{"amount_mib": 0, "deflate_on_oom": true, "stats_polling_interval_s": 5},
		"vsock":   map[string]any{"guest_cid": guestCID, "uds_path": vsockPath},
	}, "", "  ")
}
