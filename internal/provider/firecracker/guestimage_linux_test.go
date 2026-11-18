package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/guestimage"
	"github.com/asterism-dev/observatory/internal/proto"
)

func TestBaseForMissingImage(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(testImageURL))
	stock := filepath.Join(dir, hex.EncodeToString(sum[:8])+".raw")
	if err := os.WriteFile(stock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Provider{ImagesDir: dir, ImageURL: testImageURL, custom: true}
	if _, err := p.baseFor(node.VMFile{Image: "2026.11.1-deadbeef"}); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("missing guest image: err = %v", err)
	}
	if got, err := p.baseFor(node.VMFile{}); err != nil || got != stock {
		t.Fatalf("stock base = %q, %v; want %q", got, err, stock)
	}
	img := guestImagePath(dir, "2026.11.1-deadbeef")
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := p.baseFor(node.VMFile{Image: "2026.11.1-deadbeef"}); err != nil || got != img {
		t.Fatalf("guest base = %q, %v; want %q", got, err, img)
	}
}

const testImageVersion = "2026.11.1-0000test"

// stubImageProvider builds testImageVersion with script instead of the real build script, from VMM_TEST_IMAGE as the stock base.
func stubImageProvider(t *testing.T, script string) *Provider {
	t.Helper()
	p := testProvider(t)
	p.custom = false
	p.image = stubImage(script)
	waitBuildMemory()
	return p
}

// waitBuildMemory gives a small test node back the memory of the last build VM, which is freed a little after it ends, and its page cache.
func waitBuildMemory() {
	exec.Command("sync").Run()
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o200)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, err := imageBuildMemory(); err == nil {
			return
		}
	}
}

func stubImage(script string) guestimage.Image {
	return guestimage.Image{Version: testImageVersion, Manifest: guestimage.Manifest{Base: guestimage.Artifact{URL: testImageURL}}, Files: map[string][]byte{"vmm-image-build.sh": []byte(script)}}
}

const (
	stubBuildOK = `#!/bin/sh
set -eu
ip -4 addr show | grep -q 'inet 192.168.64.254/24'
install -d -m 0755 /etc/observatory
printf '%s\n' "$2" >/etc/observatory/image
sync
`
	stubBuildFail = `#!/bin/sh
echo "vmm-image-build: step docker"
echo "vmm-image-build: FAIL docker: boom"
exit 1
`
	stubBuildWrongVersion = `#!/bin/sh
install -d -m 0755 /etc/observatory
printf '2026.11.1-ffffffff\n' >/etc/observatory/image
sync
`
	stubBuildHang = `#!/bin/sh
sleep 600
`
	// Writes the right version, then reboots at once instead of powering off.
	stubBuildNoPoweroff = `#!/bin/sh
install -d -m 0755 /etc/observatory
printf '%s\n' "$2" >/etc/observatory/image
sync
echo b >/proc/sysrq-trigger
sleep 600
`
)

type imageReports struct {
	mu  sync.Mutex
	got []proto.ImageStatus
}

func (r *imageReports) report(st proto.ImageStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, st)
}

func (r *imageReports) states() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s []string
	for _, st := range r.got {
		s = append(s, st.State)
	}
	return s
}

func (r *imageReports) last() proto.ImageStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[len(r.got)-1]
}

// runUntilFailed runs RunGuestImages until it reports a failure, which would otherwise wait imageRetry.
func runUntilFailed(t *testing.T, p *Provider) *imageReports {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r := &imageReports{}
	p.RunGuestImages(ctx, func(st proto.ImageStatus) {
		r.report(st)
		if st.State == proto.ImageFailed {
			cancel()
		}
	})
	if got := r.states(); !slices.Equal(got, []string{proto.ImageBuilding, proto.ImageFailed}) {
		t.Fatalf("states = %v", got)
	}
	return r
}

func noBuildLeft(t *testing.T, p *Provider) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(p.VMsDir, "tpl-*")); len(left) != 0 {
		t.Errorf("build dirs left: %v", left)
	}
}

func noGuestImage(t *testing.T, p *Provider) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(p.ImagesDir, "guest-*")); len(left) != 0 {
		t.Errorf("guest images left: %v", left)
	}
}

func TestGuestImageCustomReportsCustom(t *testing.T) {
	p := &Provider{ImagesDir: t.TempDir(), VMsDir: t.TempDir(), custom: true, image: stubImage(stubBuildOK)}
	r := &imageReports{}
	p.RunGuestImages(context.Background(), r.report)
	if len(r.got) != 1 || r.got[0] != (proto.ImageStatus{State: proto.ImageCustom}) {
		t.Fatalf("reports = %+v", r.got)
	}
	noGuestImage(t, p)
}

func TestGuestImageReadyPrunes(t *testing.T) {
	p := &Provider{ImagesDir: t.TempDir(), VMsDir: t.TempDir(), image: stubImage(stubBuildOK)}
	now := time.Now()
	for i, v := range []string{"2026.10.1-aaaaaaaa", "2026.10.2-bbbbbbbb", testImageVersion} {
		path := guestImagePath(p.ImagesDir, v)
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(guestImagePath(p.ImagesDir, "2026.12.1-cccccccc")+".part", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &imageReports{}
	p.RunGuestImages(context.Background(), r.report)
	built := now.Add(-time.Hour)
	if len(r.got) != 1 || r.got[0].State != proto.ImageReady || r.got[0].Version != testImageVersion || r.got[0].BuiltAt == nil || !r.got[0].BuiltAt.Equal(built) {
		t.Fatalf("reports = %+v", r.got)
	}
	left, _ := filepath.Glob(filepath.Join(p.ImagesDir, "guest-*"))
	want := []string{guestImagePath(p.ImagesDir, testImageVersion), guestImagePath(p.ImagesDir, "2026.10.2-bbbbbbbb")}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

func TestGuestImageBuildRefusedWithoutSpace(t *testing.T) {
	p := &Provider{ImagesDir: t.TempDir(), VMsDir: t.TempDir(), image: stubImage(stubBuildOK)}
	old := checkSpace
	t.Cleanup(func() { checkSpace = old })
	checkSpace = func(dir string, need int64) error {
		if dir != p.VMsDir || need != imageBuildSpace {
			t.Errorf("checkSpace(%s, %d)", dir, need)
		}
		return fmt.Errorf("not enough space on node: %d MiB free, %d MiB needed", (need-1)>>20, need>>20)
	}
	r := runUntilFailed(t, p)
	if st := r.last(); !strings.Contains(st.Error, "not enough space on node") {
		t.Fatalf("error = %q", st.Error)
	}
	if entries, _ := os.ReadDir(p.VMsDir); len(entries) != 0 {
		t.Fatalf("cloned anyway: %v", entries)
	}
	noGuestImage(t, p)
}

func TestGuestImageFailedBuildReportsFallback(t *testing.T) {
	p := &Provider{ImagesDir: t.TempDir(), VMsDir: t.TempDir(), image: stubImage(stubBuildOK)}
	const prev = "2026.10.1-aaaaaaaa"
	if err := os.WriteFile(guestImagePath(p.ImagesDir, prev), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := checkSpace
	t.Cleanup(func() { checkSpace = old })
	checkSpace = func(string, int64) error { return errors.New("not enough space on node") }
	r := runUntilFailed(t, p)
	for _, st := range r.got {
		if st.Fallback != prev {
			t.Fatalf("%s: fallback = %q", st.State, st.Fallback)
		}
	}
}

func TestGuestImageBuild(t *testing.T) {
	p := stubImageProvider(t, stubBuildOK)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r := &imageReports{}
	p.RunGuestImages(ctx, func(st proto.ImageStatus) {
		r.report(st)
		if st.State == proto.ImageFailed {
			cancel()
		}
	})
	if got := r.states(); !slices.Equal(got, []string{proto.ImageBuilding, proto.ImageReady}) {
		t.Fatalf("states = %v (%+v)", got, r.got)
	}
	path := guestImagePath(p.ImagesDir, testImageVersion)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 || st.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Errorf("mode %v uid %d", st.Mode(), st.Sys().(*syscall.Stat_t).Uid)
	}
	if got, err := readGuestFile(path, guestimage.ImageFile, 64); err != nil || strings.TrimSpace(string(got)) != testImageVersion {
		t.Errorf("image file = %q, %v", got, err)
	}
	noBuildLeft(t, p)
}

func TestGuestImageBuildFailure(t *testing.T) {
	p := stubImageProvider(t, stubBuildFail)
	r := runUntilFailed(t, p)
	st := r.last()
	if !strings.Contains(st.Error, "FAIL docker: boom") {
		t.Errorf("error = %q", st.Error)
	}
	if st.RetryAt == nil || time.Until(*st.RetryAt) < imageRetry-time.Minute || time.Until(*st.RetryAt) > imageRetry {
		t.Errorf("retryAt = %v", st.RetryAt)
	}
	noGuestImage(t, p)
	noBuildLeft(t, p)
}

func TestGuestImageWrongVersionRejected(t *testing.T) {
	p := stubImageProvider(t, stubBuildWrongVersion)
	r := runUntilFailed(t, p)
	if st := r.last(); !strings.Contains(st.Error, "2026.11.1-ffffffff") {
		t.Errorf("error = %q", st.Error)
	}
	noGuestImage(t, p)
	noBuildLeft(t, p)
}

func TestGuestImageInterruptedBuildDiscarded(t *testing.T) {
	p := stubImageProvider(t, stubBuildHang)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	console := filepath.Join(node.VMDir(p.VMsDir, imageBuildID), node.ConsoleLog)
	go func() {
		// Cancel once the build VM has booted far enough to run its user-data.
		for ctx.Err() == nil {
			if b, _ := os.ReadFile(console); bytes.Contains(b, []byte("cloud-init")) {
				time.Sleep(5 * time.Second)
				cancel()
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	r := &imageReports{}
	done := make(chan struct{})
	go func() {
		p.RunGuestImages(ctx, func(st proto.ImageStatus) {
			r.report(st)
			if st.State == proto.ImageFailed {
				t.Errorf("build failed: %s", st.Error)
				cancel()
			}
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("RunGuestImages still running after cancel")
	}
	if got := r.states(); !slices.Equal(got, []string{proto.ImageBuilding}) {
		t.Fatalf("states = %v", got)
	}
	noGuestImage(t, p)
	noBuildLeft(t, p)

	part := guestImagePath(p.ImagesDir, testImageVersion) + ".part"
	if err := os.WriteFile(part, []byte("half a disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.image = stubImage(stubBuildOK)
	waitBuildMemory()
	r = &imageReports{}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p.RunGuestImages(ctx, func(st proto.ImageStatus) {
		if _, err := os.Stat(part); st.State == proto.ImageBuilding && err == nil {
			t.Error("leftover .part still there when the build starts")
		}
		if st.State == proto.ImageFailed {
			cancel()
		}
		r.report(st)
	})
	if got := r.states(); !slices.Equal(got, []string{proto.ImageBuilding, proto.ImageReady}) {
		t.Fatalf("states = %v (%+v)", got, r.got)
	}
	if _, err := os.Stat(part); err == nil {
		t.Error(".part left after the build")
	}
}

func TestGuestImageWithoutPoweroffRejected(t *testing.T) {
	p := stubImageProvider(t, stubBuildNoPoweroff)
	r := runUntilFailed(t, p)
	if st := r.last(); !strings.Contains(st.Error, "without a guest poweroff") {
		t.Errorf("error = %q", st.Error)
	}
	noGuestImage(t, p)
	noBuildLeft(t, p)
}

func TestMoveDiskAcrossFilesystems(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "root.img"), filepath.Join(dir, "guest.raw.part")
	if err := os.WriteFile(src, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := rename
	t.Cleanup(func() { rename = old })
	rename = func(o, n string) error { return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV} }
	if err := moveDisk(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("source left behind")
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	head := make([]byte, 4)
	st, _ := f.Stat()
	if _, err := f.Read(head); err != nil || string(head) != "disk" || st.Size() != imageDiskGB<<30 {
		t.Fatalf("copy = %q (%d bytes), %v", head, st.Size(), err)
	}

	rename = func(string, string) error { return errors.New("boom") }
	if err := moveDisk(dst, src); err == nil || err.Error() != "boom" {
		t.Fatalf("other rename error = %v", err)
	}
}
