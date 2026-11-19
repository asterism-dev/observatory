package firecracker

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/asterism-dev/observatory/internal/node/guestimage"
)

func guestImagePath(imagesDir, version string) string {
	return filepath.Join(imagesDir, "guest-"+version+".raw")
}

// pruneGuestImages removes every guest image and unfinished build except current and previous; VMs own their root.img clones.
func pruneGuestImages(imagesDir, current, previous string) error {
	raws, _ := filepath.Glob(filepath.Join(imagesDir, "guest-*.raw"))
	parts, _ := filepath.Glob(filepath.Join(imagesDir, "guest-*.raw.part"))
	for _, p := range append(raws, parts...) {
		if p == guestImagePath(imagesDir, current) || previous != "" && p == guestImagePath(imagesDir, previous) {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// previousGuestImage is the version of the newest guest image other than current, or "".
func previousGuestImage(imagesDir, current string) string {
	raws, _ := filepath.Glob(filepath.Join(imagesDir, "guest-*.raw"))
	var prev string
	var newest int64
	for _, p := range raws {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "guest-"), ".raw")
		st, err := os.Stat(p)
		if v == current || err != nil {
			continue
		}
		if m := st.ModTime().UnixNano(); prev == "" || m > newest {
			prev, newest = v, m
		}
	}
	return prev
}

// buildSeedFiles is the build VM's seed: the build files and a user-data that runs the build script and powers off either way.
func buildSeedFiles(img guestimage.Image, version string) map[string][]byte {
	seed := maps.Clone(img.Files)
	// users: [] keeps cloud-init's default user out of the image.
	seed["user-data"] = []byte(fmt.Sprintf(`#cloud-config
hostname: vmm-image-build
users: []
runcmd:
  - [sh, -c, 'mkdir -p /run/vmm-seed && mount -o ro LABEL=cidata /run/vmm-seed && sh /run/vmm-seed/vmm-image-build.sh /run/vmm-seed %s; poweroff']
`, version))
	seed["meta-data"] = []byte("instance-id: vmm-image-build\nlocal-hostname: vmm-image-build\n")
	// The build VM's MAC is random, and it has a single NIC.
	seed["network-config"] = []byte(fmt.Sprintf(`version: 2
ethernets:
  primary:
    match:
      name: "e*"
    addresses: [%q]
    routes: [{to: default, via: %s}]
    nameservers: {addresses: [1.1.1.1, 9.9.9.9]}
`, templateNet.Address, templateNet.Gateway))
	return seed
}
