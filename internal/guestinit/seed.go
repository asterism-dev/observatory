package guestinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nonHostRE  = regexp.MustCompile(`[^A-Za-z0-9-]`)
)

// Root is the directory all of the init's file writes go through: "/" in the VM once the init has switched into the image's root.
type Root struct{ Path string }

// ReadSeed reads and sanitizes the seed document from the seed ISO on dev.
func ReadSeed(dev string) (proto.GuestSeed, error) {
	var seed proto.GuestSeed
	raw, err := readISOFile(dev, proto.GuestSeedFile)
	if err != nil {
		return seed, err
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		return seed, fmt.Errorf("seed %s: %w", proto.GuestSeedFile, err)
	}
	if !hostnameRE.MatchString(seed.Hostname) {
		// A bad name must not stop the boot: PID 1 exiting panics the kernel.
		name := sanitizeHostname(seed.Hostname)
		fmt.Fprintf(os.Stderr, "init: seed: invalid hostname %q, using %q\n", seed.Hostname, name)
		seed.Hostname = name
	}
	seed.SSHKeys = slices.DeleteFunc(seed.SSHKeys, func(k string) bool {
		return strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\r\n")
	})
	for name := range seed.Env {
		if !envNameRE.MatchString(name) {
			delete(seed.Env, name)
		}
	}
	return seed, nil
}

// sanitizeHostname makes name a valid hostname, falling back to "vm".
func sanitizeHostname(name string) string {
	name = strings.Trim(nonHostRE.ReplaceAllString(name, ""), "-")
	name = strings.TrimRight(name[:min(len(name), 63)], "-")
	if name == "" {
		return "vm"
	}
	return name
}

// readISOFile reads the ISO as such: diskfs.Open would take a block device's 512-byte sectors as the ISO block size.
func readISOFile(dev, name string) ([]byte, error) {
	iso, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer iso.Close()
	size, err := iso.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	fs, err := iso9660.Read(file.New(iso, true), size, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("seed %s: %w", dev, err)
	}
	entries, err := fs.ReadDir(".")
	if err != nil {
		return nil, err
	}
	// go-diskfs does not wrap fs.ErrNotExist itself.
	if !slices.ContainsFunc(entries, func(e iofs.DirEntry) bool { return e.Name() == name }) {
		return nil, fmt.Errorf("seed %s: %w", name, iofs.ErrNotExist)
	}
	f, err := fs.OpenFile("/"+name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// FindSeedDevice returns the last read-only virtio disk: the node attaches the seed after root and data disks.
func FindSeedDevice(sysBlock string) (string, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "vd") {
			names = append(names, e.Name())
		}
	}
	// vdz sorts before vdaa by length first.
	slices.SortFunc(names, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	for _, n := range slices.Backward(names) {
		if ro, err := os.ReadFile(filepath.Join(sysBlock, n, "ro")); err == nil && strings.TrimSpace(string(ro)) == "1" {
			return "/dev/" + n, nil
		}
	}
	return "", errors.New("seed: no read-only virtio disk found")
}
