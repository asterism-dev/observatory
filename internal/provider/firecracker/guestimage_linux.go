package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/guestimage"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

const (
	imageBuildTimeout     = 30 * time.Minute
	imageRetry            = 30 * time.Minute
	imageDiskGB           = 10
	imageBuildMemoryMB    = 4096
	imageBuildMinMemoryMB = 1536
	// Twice the 5 GiB used-size budget; the file itself is sparse.
	imageBuildSpace = 2 * (5 << 30)
	// The tpl- prefix lets removeUnfinishedTemplates clean up a build a node stop interrupted.
	imageBuildID = "tpl-image-build"
)

// checkSpace is a variable so tests can simulate a full disk.
var checkSpace = node.CheckSpace

// RunGuestImages makes sure the current guest image exists, building it if needed, and reports its state.
func (p *Provider) RunGuestImages(ctx context.Context, report func(proto.ImageStatus)) {
	if p.custom {
		report(proto.ImageStatus{State: proto.ImageCustom})
		return
	}
	v := p.image.Version
	path := guestImagePath(p.ImagesDir, v)
	for {
		if st, err := os.Stat(path); err == nil {
			built := st.ModTime()
			report(proto.ImageStatus{Version: v, State: proto.ImageReady, BuiltAt: &built})
			if err := pruneGuestImages(p.ImagesDir, v, previousGuestImage(p.ImagesDir, v)); err != nil {
				log.Printf("guest image: prune: %v", err)
			}
			return
		}
		if err := os.Remove(path + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("guest image: %v", err)
		}
		started := time.Now()
		report(proto.ImageStatus{Version: v, State: proto.ImageBuilding, StartedAt: &started})
		err := p.buildGuestImage(ctx, p.image)
		if err == nil {
			log.Printf("guest image %s: built in %s", v, time.Since(started).Round(time.Second))
			continue
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("guest image %s: %v", v, err)
		retry := time.Now().Add(imageRetry)
		report(proto.ImageStatus{Version: v, State: proto.ImageFailed, Error: err.Error(), StartedAt: &started, RetryAt: &retry})
		select {
		case <-ctx.Done():
			return
		case <-time.After(imageRetry):
		}
	}
}

// buildGuestImage boots the stock image with a seed that runs the build script and keeps the disk once it powers off with img's version on it.
func (p *Provider) buildGuestImage(ctx context.Context, img guestimage.Image) error {
	p.building.Lock()
	defer p.building.Unlock()
	if err := checkSpace(p.ImagesDir, imageBuildSpace); err != nil {
		return err
	}
	mem, err := imageBuildMemory()
	if err != nil {
		return err
	}
	base, err := p.stockBase()
	if err != nil {
		return err
	}
	f := node.VMFile{MAC: node.RandomMAC(), Spec: proto.VMSpec{ID: imageBuildID, Name: "vmm-image-build", CPUs: 2, MemoryMB: mem, DiskGB: imageDiskGB}}
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	defer os.RemoveAll(dir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Builds and templates share the lock, so the reserved template address is free.
	if err := os.WriteFile(filepath.Join(dir, "nat-ip"), []byte(linuxvm.TemplateNATIP+"\n"), 0o644); err != nil {
		return err
	}
	root := filepath.Join(dir, "root.img")
	if err := node.CloneResize(base, root, imageDiskGB); err != nil {
		return err
	}
	if err := node.WriteSeedISO(dir, buildSeedFiles(img, img.Version)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, imageBuildTimeout)
	defer cancel()
	inst, err := p.coldBoot(ctx, f, dir)
	if err != nil {
		return err
	}
	select {
	case <-inst.Done():
	case <-ctx.Done():
		inst.Kill()
		return fmt.Errorf("image build: %w", ctx.Err())
	}
	got, err := readGuestFile(root, guestimage.ImageFile, 64)
	if v := strings.TrimSpace(string(got)); err != nil || v != img.Version {
		if line := lastFailLine(filepath.Join(dir, node.ConsoleLog)); line != "" {
			return errors.New(line)
		}
		if err == nil {
			return fmt.Errorf("build VM wrote image %q, want %q", v, img.Version)
		}
		return errors.New("build VM powered off without an image")
	}
	dst := guestImagePath(p.ImagesDir, img.Version)
	part := dst + ".part"
	if err := os.Rename(root, part); err != nil {
		return err
	}
	// The jail gave root.img to the build VM's UID.
	if err := os.Chown(part, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(part, 0o644); err != nil {
		return err
	}
	return os.Rename(part, dst)
}

// imageBuildMemory is half the available memory, at most imageBuildMemoryMB, in 256 MiB steps.
func imageBuildMemory() (int, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	var availKB int
	for _, l := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(l, "MemAvailable:"); ok {
			availKB, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
		}
	}
	mem := min(imageBuildMemoryMB, availKB>>10/2) &^ 255
	if mem < imageBuildMinMemoryMB {
		return 0, fmt.Errorf("not enough free memory for the image build: %d MiB available, %d MiB needed", availKB>>10, 2*imageBuildMinMemoryMB)
	}
	return mem, nil
}

// lastFailLine is the build script's last failure message on the console, without the kernel or cloud-init prefix.
func lastFailLine(console string) string {
	b, err := os.ReadFile(console)
	if err != nil {
		return ""
	}
	var last string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if i := strings.Index(sc.Text(), "vmm-image-build: FAIL "); i >= 0 {
			last = strings.TrimSpace(sc.Text()[i:])
		}
	}
	return last
}
