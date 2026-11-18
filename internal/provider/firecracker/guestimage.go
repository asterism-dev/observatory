package firecracker

import (
	"os"
	"path/filepath"
	"strings"
)

func guestImagePath(imagesDir, version string) string {
	return filepath.Join(imagesDir, "guest-"+version+".raw")
}

// pruneGuestImages removes every guest image and unfinished build except current and previous; VMs own their root.img clones.
func pruneGuestImages(imagesDir, current, previous string) error {
	raws, _ := filepath.Glob(filepath.Join(imagesDir, "guest-*.raw"))
	parts, _ := filepath.Glob(filepath.Join(imagesDir, "guest-*.raw.part"))
	keep := []string{guestImagePath(imagesDir, current), guestImagePath(imagesDir, previous)}
	for _, p := range append(raws, parts...) {
		if p == keep[0] || p == keep[1] {
			continue
		}
		if err := os.Remove(p); err != nil {
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
