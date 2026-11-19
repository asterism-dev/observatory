package registry

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PlatformDigest returns the manifest digest of ref, of the linux/amd64 child for an index.
func PlatformDigest(ctx context.Context, ref name.Reference, opts ...remote.Option) (string, error) {
	desc, err := remote.Get(ref, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	if !desc.MediaType.IsIndex() {
		return desc.Digest.String(), nil
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	for _, m := range im.Manifests {
		if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			return m.Digest.String(), nil
		}
	}
	return "", fmt.Errorf("%s has no linux/amd64 image", ref)
}
