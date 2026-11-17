package node

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"github.com/asterism-dev/observatory/internal/node/guest"
	"github.com/asterism-dev/observatory/internal/proto"
)

const SeedISO = "seed.iso"

// GuestSeedFiles is the cloud-init NoCloud seed of a guest VM; a nil f.Boot gives a guest without hub-managed keys and variables.
func GuestSeedFiles(f VMFile, primary PrimaryNet) map[string][]byte {
	var boot proto.Boot
	if f.Boot != nil {
		boot = *f.Boot
	}
	env, _ := json.Marshal(boot.Env) // a map[string]string always marshals
	var keys strings.Builder
	for _, k := range boot.SSHPublicKeys {
		// One key per line: a line break inside a key would smuggle in another one.
		if !strings.ContainsAny(k, "\r\n") {
			keys.WriteString(k + "\n")
		}
	}
	userData, metaData, networkConfig := CloudInit(f, primary)
	return map[string][]byte{
		"user-data": []byte(userData), "meta-data": []byte(metaData),
		"network-config": []byte(networkConfig), "vmm-disks.sh": []byte(DiskScript(f)),
		"vmm-boot.sh": []byte(guest.BootScript), "vmm-env": env, "vmm-authorized-keys": []byte(keys.String()),
	}
}

// ReadSeedFile returns one file of dir's seed.iso.
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
	f, err := fs.OpenFile("/"+name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// SeedBoot recovers the keys and variables last written into dir's seed.iso, so a reseed without a pushed Boot keeps them.
func SeedBoot(dir string) (*proto.Boot, error) {
	env, err := ReadSeedFile(dir, "vmm-env")
	if err != nil {
		return nil, err
	}
	keys, err := ReadSeedFile(dir, "vmm-authorized-keys")
	if err != nil {
		return nil, err
	}
	var b proto.Boot
	if err := json.Unmarshal(env, &b.Env); err != nil {
		return nil, err
	}
	for k := range strings.Lines(string(keys)) {
		b.SSHPublicKeys = append(b.SSHPublicKeys, strings.TrimSuffix(k, "\n"))
	}
	return &b, nil
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
