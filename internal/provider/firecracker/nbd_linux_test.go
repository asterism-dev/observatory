package firecracker

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

func TestStartWithNBDDisk(t *testing.T) {
	url, psk := os.Getenv("VMM_TEST_NBD"), os.Getenv("VMM_TEST_NBD_PSK")
	if url == "" {
		t.Skip("needs VMM_TEST_NBD and VMM_TEST_NBD_PSK")
	}
	p := testProvider(t)
	f := testVM("fcnbd")
	// The export's PSK identity is the disk ID, so it must match the URL.
	diskID := path.Base(url)
	f.Spec.Attachments = []proto.DiskAttachment{{DiskID: diskID, MountPath: "/data", URL: url, PSK: psk}}
	inst := startVM(t, p, f)
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		inst.Kill()
		t.Fatal(err)
	}
	disks, err := p.Disks(f, dir)
	if err != nil || len(disks) != 3 || disks[1].Name != diskID || disks[1].Device != "vdb" || disks[1].Error != "" {
		inst.Kill()
		t.Fatalf("disks = %+v, %v", disks, err)
	}
	b, _ := os.ReadFile(linuxvm.NBDRecord(dir, diskID))
	dev := strings.TrimSpace(string(b))
	inst.Stop()
	<-inst.Done()
	if linuxvm.NBDConnected(dev) {
		t.Fatalf("%s still connected after the VM stopped", dev)
	}
}
