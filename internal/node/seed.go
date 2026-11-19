package node

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"github.com/asterism-dev/observatory/internal/proto"
)

const SeedISO = "seed.iso"

//go:embed guestcontext/AGENTS.md
var contextFile string

// GuestSeedFiles is the seed of a guest VM for its init; a nil f.Boot gives a guest without hub-managed keys and variables.
func GuestSeedFiles(f VMFile, primary PrimaryNet) map[string][]byte {
	var boot proto.Boot
	if f.Boot != nil {
		boot = *f.Boot
	}
	seed := proto.GuestSeed{
		Hostname: f.Spec.Name, InstanceID: f.Spec.ID, Env: boot.Env, EgressCAs: boot.EgressCAs, Context: contextFile, Image: f.ImageConfig,
		NICs: []proto.GuestNIC{{MAC: f.MAC, Name: "eth0", Address: primary.Address, Gateway: primary.Gateway, DHCP: primary.Address == ""}},
	}
	for _, k := range boot.SSHPublicKeys {
		// A line break inside a key would smuggle in another one.
		if k = strings.TrimSpace(k); k != "" && !strings.ContainsAny(k, "\r\n") {
			seed.SSHKeys = append(seed.SSHKeys, k)
		}
	}
	netIdx := 0
	for _, n := range f.Spec.NICs {
		// OVN NICs take their address from OVN DHCP.
		nic := proto.GuestNIC{MAC: n.MAC, Name: "mgmt0", DHCP: true, Mgmt: true}
		if n.Network != "mgmt" {
			nic.Name, nic.Mgmt = fmt.Sprintf("net%d", netIdx), false
			netIdx++
		}
		seed.NICs = append(seed.NICs, nic)
	}
	for i, d := range AttachOrder(f) {
		if d.NBD {
			seed.Disks = append(seed.Disks, proto.GuestDisk{Device: "/dev/" + DeviceName(i), MountPath: f.Spec.Attachments[i-1].MountPath})
		}
	}
	b, _ := json.Marshal(seed) // plain strings, slices and maps always marshal
	return map[string][]byte{proto.GuestSeedFile: b}
}

// ReadSeedFile returns one file of dir's seed.iso; a missing file gives an error wrapping fs.ErrNotExist.
func ReadSeedFile(dir, name string) ([]byte, error) {
	d, err := diskfs.Open(filepath.Join(dir, SeedISO), diskfs.WithOpenMode(diskfs.ReadOnly))
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

// SeedBoot recovers the keys, variables and egress CAs last written into dir's seed.iso, so a reseed without a pushed Boot keeps them.
func SeedBoot(dir string) (*proto.Boot, error) {
	b, err := ReadSeedFile(dir, proto.GuestSeedFile)
	if errors.Is(err, iofs.ErrNotExist) {
		return &proto.Boot{}, nil
	}
	if err != nil {
		return nil, err
	}
	var seed proto.GuestSeed
	if err := json.Unmarshal(b, &seed); err != nil {
		return nil, err
	}
	return &proto.Boot{SSHPublicKeys: seed.SSHKeys, Env: seed.Env, EgressCAs: seed.EgressCAs}, nil
}

// WriteSeedISO builds beside dir/seed.iso and renames, so a failed build keeps the old seed bootable.
func WriteSeedISO(dir string, files map[string][]byte) (err error) {
	tmp := filepath.Join(dir, "seed-new.iso")
	os.Remove(tmp)
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	work, err := os.MkdirTemp("", "vmm-seed")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	size := int64(1 << 20)
	for _, b := range files {
		size += int64(len(b)) + 2048
	}
	size = (size + 2047) &^ 2047
	d, err := diskfs.Create(tmp, size, diskfs.SectorSize(2048))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{FSType: filesystem.TypeISO9660, VolumeLabel: "cidata", WorkDir: work})
	if err != nil {
		return err
	}
	for name, b := range files {
		f, err := fs.OpenFile("/"+name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return errors.New("seed: unexpected filesystem type")
	}
	if err := iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: "cidata"}); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, SeedISO))
}
