package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

const (
	maxTemplates   = 4
	templateDiskGB = 10
	// templateMarker in a VM dir names the template Prepare cloned root.img from; the first Start restores that template.
	templateMarker = "from-template"
	templatesDir   = ".templates"
)

// templateKey identifies the template f can start from: same image, kernel and init, Firecracker, vCPUs, RAM, NIC count, seed and boot config.
func templateKey(f node.VMFile, fcVersion, bootID string) (string, error) {
	digest, err := imageDigest(f.Image)
	if err != nil {
		return "", err
	}
	t := templateFile("tpl", f)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", digest, bootID, fcVersion)
	seed := node.GuestSeedFiles(t, templateNet)
	for _, n := range slices.Sorted(maps.Keys(seed)) {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(seed[n]))
		h.Write(seed[n])
	}
	var drives []string
	for _, d := range node.AttachOrder(t) {
		drives = append(drives, "/"+d.File)
	}
	cfg, err := vmConfig(t, "/vmlinux", "/initramfs.cpio", drives)
	if err != nil {
		return "", err
	}
	h.Write(cfg)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

var templateNet = node.PrimaryNet{Address: linuxvm.TemplateNATIP + "/24", Gateway: linuxvm.NATGateway}

// templateFile is the VM a template of f's shape is built from: no keys, variables, attachments or OVN ports.
func templateFile(id string, f node.VMFile) node.VMFile {
	t := node.VMFile{MAC: "02:00:00:00:00:00", Image: f.Image, ImageConfig: f.ImageConfig, Spec: proto.VMSpec{ID: id, Name: "template", CPUs: f.Spec.CPUs, MemoryMB: f.Spec.MemoryMB, DiskGB: templateDiskGB}}
	for i := range f.Spec.NICs {
		// No OVN logical port has this name (the hub names ports after VM ids), so the template never joins a network.
		t.Spec.NICs = append(t.Spec.NICs, proto.NIC{Network: "template", MAC: "02:00:00:00:00:00", Port: fmt.Sprintf("%s-%d", id, i)})
	}
	return t
}

// evictTemplates removes the least recently used templates in dir beyond keep; using a template touches its mtime.
func evictTemplates(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type tpl struct {
		path string
		mod  int64
	}
	var tpls []tpl
	for _, e := range entries {
		if !e.IsDir() || filepath.Ext(e.Name()) == ".tmp" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		tpls = append(tpls, tpl{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	sort.Slice(tpls, func(i, j int) bool { return tpls[i].mod > tpls[j].mod })
	for len(tpls) > keep {
		if err := os.RemoveAll(tpls[len(tpls)-1].path); err != nil {
			return err
		}
		tpls = tpls[:len(tpls)-1]
	}
	return nil
}
