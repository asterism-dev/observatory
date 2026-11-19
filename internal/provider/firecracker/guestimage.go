package firecracker

import (
	"fmt"
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
