package firecracker

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/asterism-dev/observatory/internal/version"
)

// StandardImage is the guest image VMs boot; its tag is the node's version.
const StandardImage = "ghcr.io/asterism-dev/observatory-guest"

// guestImageRef is configured, or the standard image of this node's version (latest for unreleased builds).
func guestImageRef(configured string) string {
	if configured != "" {
		return configured
	}
	tag := version.Version
	if !version.Released(tag) {
		tag = "latest"
	}
	return StandardImage + ":" + tag
}

func pinned(ref, digest string) string {
	name, _, _ := strings.Cut(ref, "@")
	return name + "@" + digest
}

func imageDigest(image string) (string, error) {
	_, digest, ok := strings.Cut(image, "@")
	if !ok {
		return "", fmt.Errorf("image %q has no digest", image)
	}
	return digest, nil
}

// legacyImageRE matches what the old runtime left in ImagesDir: guest disks and base images converted from download URLs.
var legacyImageRE = regexp.MustCompile(`^(guest-.*\.raw(\.part)?|[0-9a-f]{16}\.raw(\.part|\.download)?)$`)

// removeLegacyImages frees the old runtime's images; legacy VMs boot from their own root.img clones.
func removeLegacyImages(imagesDir string) {
	entries, _ := os.ReadDir(imagesDir)
	for _, e := range entries {
		if e.Type().IsRegular() && legacyImageRE.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(imagesDir, e.Name())); err != nil {
				slog.Warn("remove legacy image failed", "err", err)
			}
		}
	}
}
