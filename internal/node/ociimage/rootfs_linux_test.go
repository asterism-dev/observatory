package ociimage

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func needRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	for _, tool := range []string{"mke2fs", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
}

func tarLayer(t *testing.T, hdrs ...*tar.Header) v1.Layer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(1700000000, 0)
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(h.Name))
		}
	}
	tw.Close()
	data := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// diskFrom pushes an amd64 image built from layers, pulls it and builds its disk.
func diskFrom(t *testing.T, s Store, layers ...v1.Layer) (string, string) {
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	ref := testRegistry(t) + "/img:1"
	if err := remote.Write(mustRef(t, ref), img); err != nil {
		t.Fatal(err)
	}
	p, err := s.Pull(context.Background(), ref, authn.Anonymous)
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Disk(context.Background(), p.Digest)
	if err != nil {
		t.Fatal(err)
	}
	return path, p.Digest
}

func debugfs(t *testing.T, disk, cmd string) string {
	out, err := exec.Command("debugfs", "-R", cmd, disk).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs %s: %v\n%s", cmd, err, out)
	}
	return string(out)
}

func storeDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/var/tmp", "ociimage-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestDiskKeepsSetuid(t *testing.T) {
	needRoot(t)
	disk, _ := diskFrom(t, Store{Dir: storeDir(t)}, tarLayer(t,
		&tar.Header{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755},
		&tar.Header{Name: "home/dev/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 1000, Gid: 1000},
		&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777},
		&tar.Header{Name: "usr/bin/ping", Typeflag: tar.TypeReg, Mode: 0o755,
			PAXRecords: map[string]string{"SCHILY.xattr.user.test": "kept"}},
		&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
		&tar.Header{Name: "etc/link", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin/sudo"},
	))
	sudo := debugfs(t, disk, "stat /usr/bin/sudo")
	for _, want := range []string{"Mode:  04755", "User:     0   Group:     0"} {
		if !strings.Contains(sudo, want) {
			t.Fatalf("sudo lacks %q:\n%s", want, sudo)
		}
	}
	checks := map[string][]string{
		"stat /home/dev":        {"User:  1000   Group:  1000", "Mode:  0750"},
		"stat /tmp":             {"Mode:  01777"},
		"stat /dev/null":        {"Type: character special", "Device major/minor number: 01:03"},
		"stat /etc/link":        {"Type: symlink", `Fast link dest: "/usr/bin/sudo"`},
		"ea_list /usr/bin/ping": {"user.test", "kept"},
	}
	for cmd, wants := range checks {
		out := debugfs(t, disk, cmd)
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", cmd, want, out)
			}
		}
	}
	if out := debugfs(t, disk, "stat /usr/bin/sudo"); !strings.Contains(out, "mtime: 0x6553f100") {
		t.Errorf("sudo mtime not kept:\n%s", out)
	}
}

func TestDiskWhiteouts(t *testing.T) {
	needRoot(t)
	disk, _ := diskFrom(t, Store{Dir: storeDir(t)},
		tarLayer(t,
			&tar.Header{Name: "gone", Typeflag: tar.TypeReg, Mode: 0o644},
			&tar.Header{Name: "opq/", Typeflag: tar.TypeDir, Mode: 0o755},
			&tar.Header{Name: "opq/old", Typeflag: tar.TypeReg, Mode: 0o644},
			&tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644},
			&tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"},
		),
		tarLayer(t,
			&tar.Header{Name: ".wh.gone", Typeflag: tar.TypeReg},
			&tar.Header{Name: "opq/", Typeflag: tar.TypeDir, Mode: 0o755},
			&tar.Header{Name: "opq/.wh..wh..opq", Typeflag: tar.TypeReg},
			&tar.Header{Name: "opq/new", Typeflag: tar.TypeReg, Mode: 0o644},
		),
	)
	if out := debugfs(t, disk, "stat /gone"); !strings.Contains(out, "File not found") {
		t.Errorf("whited-out file present:\n%s", out)
	}
	ls := debugfs(t, disk, "ls /opq")
	if strings.Contains(ls, "old") || !strings.Contains(ls, "new") {
		t.Errorf("opaque dir wrong:\n%s", ls)
	}
	inode := regexp.MustCompile(`Inode: (\d+)`)
	a, b := debugfs(t, disk, "stat /a"), debugfs(t, disk, "stat /b")
	if ia, ib := inode.FindStringSubmatch(a), inode.FindStringSubmatch(b); ia == nil || ib == nil || ia[1] != ib[1] {
		t.Errorf("hardlink pair has different inodes:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "Links: 2") {
		t.Errorf("link count:\n%s", a)
	}
}

func TestDiskIdempotent(t *testing.T) {
	needRoot(t)
	s := Store{Dir: storeDir(t)}
	path, digest := diskFrom(t, s, tarLayer(t, &tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644}))

	os.Remove(path)
	var wg sync.WaitGroup
	paths, errs := make([]string, 4), make([]error, 4)
	for i := range paths {
		wg.Go(func() { paths[i], errs[i] = s.Disk(context.Background(), digest) })
	}
	wg.Wait()
	for i := range paths {
		if errs[i] != nil || paths[i] != path {
			t.Fatalf("concurrent Disk %d: %q %v", i, paths[i], errs[i])
		}
	}

	orig := mke2fs
	mke2fs = "/nonexistent/mke2fs"
	t.Cleanup(func() { mke2fs = orig })
	got, err := s.Disk(context.Background(), digest)
	if err != nil || got != path {
		t.Fatalf("cached Disk: %q %v", got, err)
	}
}

func TestDiskRejectsEscapes(t *testing.T) {
	needRoot(t)
	s := Store{Dir: storeDir(t)}
	outside := storeDir(t)
	if err := os.WriteFile(outside+"/secret", []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, tarLayer(t,
		&tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: outside},
		&tar.Header{Name: "stolen", Typeflag: tar.TypeLink, Linkname: "esc/secret"},
	))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := img.Digest()
	if err := pullLocal(s, img); err != nil {
		t.Fatal(err)
	}
	_, err = s.Disk(context.Background(), h.String())
	if err == nil || !strings.Contains(err.Error(), "parent esc/: not a directory") {
		t.Fatalf("hardlink through symlink: %v", err)
	}
	if st, _ := os.Stat(outside + "/secret"); st.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Fatal("host file got linked into the image")
	}
}

// pullLocal stores img's blobs as Pull would, without the platform check.
func pullLocal(s Store, img v1.Image) error {
	if err := os.MkdirAll(s.blobDir(), 0o700); err != nil {
		return err
	}
	layers, _ := img.Layers()
	for _, l := range layers {
		d, _ := l.Digest()
		rc, _ := l.Compressed()
		if err := s.writeBlob(d, rc); err != nil {
			return err
		}
	}
	m, _ := img.Manifest()
	cfg, _ := img.RawConfigFile()
	raw, _ := img.RawManifest()
	d, _ := img.Digest()
	if err := s.writeBlob(m.Config.Digest, bytes.NewReader(cfg)); err != nil {
		return err
	}
	return s.writeBlob(d, bytes.NewReader(raw))
}
