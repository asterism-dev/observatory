package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestAttachOrder(t *testing.T) {
	f := VMFile{Spec: proto.VMSpec{Attachments: []proto.DiskAttachment{{DiskID: "d1", URL: "nbd://h:1/d1.img", PSK: "k"}}}}
	got := AttachOrder(f)
	want := []AttachedDisk{
		{Name: "root", File: "root.img"},
		{Name: "d1", File: "nbd://h:1/d1.img", PSK: "k", NBD: true},
		{Name: "seed", File: SeedISO, ReadOnly: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("disk %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if DeviceName(0) != "vda" || DeviceName(3) != "vdd" {
		t.Errorf("DeviceName = %s %s", DeviceName(0), DeviceName(3))
	}
}

func TestDiskAttachmentPSKNotInStatus(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"root.img", SeedISO} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	psk := strings.Repeat("5a", 32)
	f := VMFile{Spec: proto.VMSpec{Attachments: []proto.DiskAttachment{{DiskID: "d1", URL: "nbd://10.198.0.1:10810/d1", PSK: psk}}}}
	disks, err := ListDisks(f, dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(disks)
	if strings.Contains(string(b), psk) {
		t.Fatalf("status carries the key: %s", b)
	}
}
