package ociimage

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"golang.org/x/sys/unix"
)

var mke2fs = "mke2fs"

// hostFree reports free bytes, free inodes and total inodes of dir's filesystem; tests replace it.
var hostFree = func(dir string) (bytes, inodes, totalInodes int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, 0, err
	}
	return int64(st.Bavail) * st.Frsize, int64(st.Ffree), int64(st.Files), nil
}

// fsSpace reports available and total bytes of dir's filesystem; tests replace it.
var fsSpace = statfsSpace

func statfsSpace(dir string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * st.Frsize, int64(st.Blocks) * st.Frsize, nil
}

// checkHost fails when bytes and inodes would eat into the reserves; btrfs-style dynamic inodes report zero total.
func checkHost(dir string, bytes, inodes int64) error {
	free, ffree, files, err := hostFree(dir)
	if err != nil {
		return err
	}
	if need := bytes + spaceReserve; free < need {
		return fmt.Errorf("not enough space on node: %d MiB free, %d MiB needed", free>>20, need>>20)
	}
	if need := inodes + files/10; files > 0 && ffree < need {
		return fmt.Errorf("not enough inodes on node: %d free, %d needed", ffree, need)
	}
	return nil
}

// Disk returns the ext4 root disk for a pulled image, building it on first use.
func (s Store) Disk(ctx context.Context, digest string) (string, error) {
	h, err := parseDigest(digest)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.Dir, "disks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Join(dir, h.Hex)
	final := base + ".raw"
	unlock, err := lockFile(base + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Stat(final); err == nil {
		now := time.Now()
		return final, os.Chtimes(final, now, now)
	}
	tree, part := base+".tmp", final+".part"
	// Leftovers of a build that crashed; we hold the lock, so nobody else is using them.
	if err := os.RemoveAll(tree); err != nil {
		return "", err
	}
	if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	defer os.RemoveAll(tree)
	img, err := s.image(h)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(tree, 0o755); err != nil {
		return "", err
	}
	used, err := unpack(ctx, img, tree)
	if err != nil {
		return "", fmt.Errorf("unpack %s: %w", digest, err)
	}
	if err := checkHost(dir, used.bytes, 1); err != nil {
		return "", err
	}
	size := diskSize(used.bytes)
	args := []string{"-q", "-t", "ext4", "-d", tree, "-E", "root_owner=0:0", "-L", "root"}
	// mke2fs allots one inode per 16 KiB; images of many small files need more than that.
	if need := used.inodes + used.inodes/5; need > size/16384 {
		args = append(args, "-N", strconv.FormatInt(need, 10))
	}
	args = append(args, part, strconv.FormatInt(size>>20, 10)+"M")
	if out, err := exec.CommandContext(ctx, mke2fs, args...).CombinedOutput(); err != nil {
		os.Remove(part)
		return "", fmt.Errorf("mke2fs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := syncFile(part); err != nil {
		return "", err
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

// diskSize is max(used×1.2, used+512 MiB) rounded up to 256 MiB.
func diskSize(used int64) int64 {
	size := max(used*6/5, used+512<<20)
	const step = 256 << 20
	return (size + step - 1) / step * step
}

func lockFile(p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

type usage struct{ bytes, inodes int64 }

// unpack writes the flattened image below root, resolving every path with O_NOFOLLOW so no planted symlink leads outside it.
func unpack(ctx context.Context, img v1.Image, root string) (usage, error) {
	var u usage
	rootFd, err := unix.Open(root, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return u, err
	}
	defer unix.Close(rootFd)
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	// Hardlinks may point at entries later in the stream; dir times change while children are added.
	var links, dirs []*tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return u, err
		}
		if err := ctx.Err(); err != nil {
			return u, err
		}
		if hdr.Name, err = cleanPath(hdr.Name); err != nil {
			return u, err
		}
		if hdr.Name == "." {
			continue
		}
		var need int64
		switch hdr.Typeflag {
		case tar.TypeLink:
			links = append(links, hdr)
			continue
		case tar.TypeDir:
			dirs = append(dirs, hdr)
			need = 4096
		case tar.TypeReg:
			need = (hdr.Size + 4095) &^ 4095
		case tar.TypeSymlink:
			if len(hdr.Linkname) >= 60 { // shorter targets live in the inode
				need = (int64(len(hdr.Linkname)) + 4095) &^ 4095
			}
		}
		u.bytes += need
		u.inodes++
		if err := checkHost(root, need, 1); err != nil {
			return u, err
		}
		if err := create(rootFd, hdr, tr); err != nil {
			return u, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	for _, hdr := range links {
		if err := link(rootFd, hdr); err != nil {
			return u, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	for _, hdr := range dirs {
		if err := withParent(rootFd, hdr.Name, func(pfd int, name string) error { return setTimes(pfd, name, hdr) }); err != nil {
			return u, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	return u, nil
}

// cleanPath roots absolute names at the image root and rejects names leaving it.
func cleanPath(name string) (string, error) {
	p := path.Clean("/" + name)
	for _, c := range strings.Split(name, "/") {
		if c == ".." {
			return "", fmt.Errorf("tar entry %q leaves the root", name)
		}
	}
	if p == "/" {
		return ".", nil
	}
	return p[1:], nil
}

// withParent opens the parent of rel below rootFd without following symlinks, creating missing directories.
func withParent(rootFd int, rel string, fn func(pfd int, name string) error) error {
	dir, name := path.Split(rel)
	fd, err := unix.Dup(rootFd)
	if err != nil {
		return err
	}
	for _, c := range strings.Split(strings.TrimSuffix(dir, "/"), "/") {
		if c == "" {
			continue
		}
		next, err := openDir(fd, c)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, c, 0o755); err == nil {
				next, err = openDir(fd, c)
			}
		}
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("parent %s: %w", dir, err)
		}
		fd = next
	}
	defer unix.Close(fd)
	return fn(fd, name)
}

func openDir(dirfd int, name string) (int, error) {
	return unix.Openat(dirfd, name, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

func create(rootFd int, hdr *tar.Header, r io.Reader) error {
	return withParent(rootFd, hdr.Name, func(pfd int, name string) error {
		perm := uint32(hdr.Mode) & 0o7777
		switch hdr.Typeflag {
		case tar.TypeDir:
			err := unix.Mkdirat(pfd, name, 0o700)
			if errors.Is(err, unix.EEXIST) {
				var st unix.Stat_t
				if err = unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
					err = errors.New("exists and is not a directory")
				}
			}
			if err != nil {
				return err
			}
		case tar.TypeReg:
			fd, err := unix.Openat(pfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err != nil {
				return err
			}
			f := os.NewFile(uintptr(fd), name)
			_, err = io.Copy(f, r)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := unix.Symlinkat(hdr.Linkname, pfd, name); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			kind := map[byte]uint32{tar.TypeChar: unix.S_IFCHR, tar.TypeBlock: unix.S_IFBLK, tar.TypeFifo: unix.S_IFIFO}[hdr.Typeflag]
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := unix.Mknodat(pfd, name, kind|0o600, int(dev)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported tar entry type %q", hdr.Typeflag)
		}
		// chown clears setuid/setgid, so the mode is set after it.
		if err := unix.Fchownat(pfd, name, hdr.Uid, hdr.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeSymlink {
			// Not a symlink (we just created it), so following is safe; Linux lacks fchmodat(AT_SYMLINK_NOFOLLOW).
			if err := unix.Fchmodat(pfd, name, perm, 0); err != nil {
				return err
			}
		}
		for k, v := range hdr.PAXRecords {
			attr, ok := strings.CutPrefix(k, "SCHILY.xattr.")
			if !ok {
				continue
			}
			// The /proc/self/fd path pins the already-opened parent; lsetxattr does not follow name.
			if err := unix.Lsetxattr(fmt.Sprintf("/proc/self/fd/%d/%s", pfd, name), attr, []byte(v), 0); err != nil {
				return fmt.Errorf("xattr %s: %w", attr, err)
			}
		}
		if hdr.Typeflag == tar.TypeDir {
			return nil
		}
		return setTimes(pfd, name, hdr)
	})
}

func link(rootFd int, hdr *tar.Header) error {
	target, err := cleanPath(hdr.Linkname)
	if err != nil {
		return err
	}
	return withParent(rootFd, target, func(tfd int, tname string) error {
		return withParent(rootFd, hdr.Name, func(pfd int, name string) error {
			return unix.Linkat(tfd, tname, pfd, name, 0)
		})
	})
}

func setTimes(pfd int, name string, hdr *tar.Header) error {
	atime := hdr.AccessTime
	if atime.IsZero() {
		atime = hdr.ModTime
	}
	ts := []unix.Timespec{timespec(atime), timespec(hdr.ModTime)}
	return unix.UtimesNanoAt(pfd, name, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func timespec(t time.Time) unix.Timespec { return unix.NsecToTimespec(t.UnixNano()) }
