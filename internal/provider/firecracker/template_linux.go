package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

const templateBuildTimeout = 10 * time.Minute

func (p *Provider) templateDir(key string) string {
	return filepath.Join(p.VMsDir, templatesDir, key)
}

// buildTemplate boots a VM of f's shape with a seed holding no keys, variables or networks and saves it once its agent answers.
func (p *Provider) buildTemplate(ctx context.Context, key string, f node.VMFile) error {
	if !p.building.TryLock() {
		return nil
	}
	defer p.building.Unlock()
	dst := p.templateDir(key)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := node.CheckSpace(p.VMsDir, int64(f.Spec.MemoryMB)<<20+templateDiskGB<<30+1<<30); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, templateBuildTimeout)
	defer cancel()
	p.mu.Lock()
	base, err := node.EnsureBaseImage(p.ImagesDir, p.ImageURL)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	t := node.VMFile{MAC: node.RandomMAC(), Spec: proto.VMSpec{ID: "tpl-" + key[:8], Name: "template", CPUs: f.Spec.CPUs, MemoryMB: f.Spec.MemoryMB, DiskGB: templateDiskGB}}
	for i := range f.Spec.NICs {
		// No OVN logical port has this name (the hub names ports after VM ids), so the template never joins a network.
		t.Spec.NICs = append(t.Spec.NICs, proto.NIC{Network: "template", MAC: node.RandomMAC(), Port: fmt.Sprintf("%s-%d", t.Spec.ID, i)})
	}
	// The build dir holds the template VM's NAT address; removing it frees the address.
	dir := node.VMDir(p.VMsDir, t.Spec.ID)
	defer os.RemoveAll(dir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := node.CloneResize(base, filepath.Join(dir, "root.img"), templateDiskGB); err != nil {
		return err
	}
	began := time.Now()
	inst, err := p.Start(t, dir)
	if err != nil {
		return err
	}
	defer func() { inst.Kill(); <-inst.Done() }()
	if err := waitAgent(ctx, dir, inst); err != nil {
		return err
	}
	if err := saveTemplate(inst, dir, dst); err != nil {
		return err
	}
	log.Printf("template %s: built in %s", key, time.Since(began).Round(time.Second))
	return evictTemplates(filepath.Dir(dst), maxTemplates)
}

// waitAgent pings the guest agent until it answers, which it does once cloud-init has finished.
func waitAgent(ctx context.Context, dir string, inst node.Instance) error {
	for {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := node.CallGuest(cctx, filepath.Join(jailRoot(dir), vsockPath), node.GuestRequest{Op: "ping"})
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent: %w", errors.Join(ctx.Err(), err))
		case <-inst.Done():
			return errors.New("template VM exited")
		case <-time.After(2 * time.Second):
		}
	}
}

// saveTemplate writes inst's memory and root disk to dst as root-owned 0644 files, shared read-only by every VM started from it.
func saveTemplate(inst node.Instance, dir, dst string) error {
	tmp := dst + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	err := inst.SaveState(tmp)
	if err == nil {
		err = os.Rename(filepath.Join(dir, "root.img"), filepath.Join(tmp, "root.img"))
	}
	for _, n := range []string{node.StateFile, node.MemFile, "root.img"} {
		if err == nil {
			err = os.Chown(filepath.Join(tmp, n), 0, 0)
		}
		if err == nil {
			err = os.Chmod(filepath.Join(tmp, n), 0o644)
		}
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.RemoveAll(tmp)
	}
	return err
}

// removeUnfinishedTemplates drops builds a node stop interrupted.
func (p *Provider) removeUnfinishedTemplates() {
	dirs, _ := filepath.Glob(filepath.Join(p.VMsDir, "tpl-*"))
	tmps, _ := filepath.Glob(filepath.Join(p.VMsDir, templatesDir, "*.tmp"))
	for _, d := range append(dirs, tmps...) {
		if err := os.RemoveAll(d); err != nil {
			log.Printf("remove unfinished template %s: %v", d, err)
		}
	}
}
