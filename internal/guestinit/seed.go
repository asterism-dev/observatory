package guestinit

import (
	"encoding/json"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"

	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Root is the directory all of the init's file writes go through: /newroot in the VM.
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
		return seed, fmt.Errorf("seed: invalid hostname %q", seed.Hostname)
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

func readISOFile(dev, name string) ([]byte, error) {
	d, err := diskfs.Open(dev, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	fs, err := d.GetFilesystem(0)
	if err != nil {
		return nil, err
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
