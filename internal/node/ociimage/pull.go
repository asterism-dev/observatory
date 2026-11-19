// Package ociimage pulls OCI images into a content-addressed cache on the node and turns them into ext4 root disks.
package ociimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

// spaceReserve stays free on the node while pulling and unpacking, so a huge image fails instead of filling the disk.
const spaceReserve = 256 << 20

// metadataLimit caps manifests and configs, which are read into memory whole.
const metadataLimit = 4 << 20

// Store keeps manifests, configs and layers in Dir/blobs/sha256/ and built disks in Dir/disks/.
type Store struct{ Dir string }

type Pulled struct {
	Digest    string // platform image manifest digest, the key for Disk
	Config    proto.ImageConfig
	SizeBytes int64 // compressed layers
}

func (s Store) Pull(ctx context.Context, ref string, auth authn.Authenticator) (Pulled, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return Pulled{}, err
	}
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithAuth(auth))
	if err != nil {
		return Pulled{}, err
	}
	if desc.Size > metadataLimit {
		return Pulled{}, fmt.Errorf("manifest of %d bytes exceeds %d", desc.Size, metadataLimit)
	}
	img, err := amd64Image(desc)
	if err != nil {
		return Pulled{}, err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return Pulled{}, err
	}
	if manifest.Config.Size > metadataLimit {
		return Pulled{}, fmt.Errorf("image config of %d bytes exceeds %d", manifest.Config.Size, metadataLimit)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return Pulled{}, err
	}
	if p := cfg.Platform(); p == nil || p.OS != "linux" || p.Architecture != "amd64" {
		return Pulled{}, fmt.Errorf("image platform %s/%s is not linux/amd64", cfg.OS, cfg.Architecture)
	}
	digest, err := img.Digest()
	if err != nil {
		return Pulled{}, err
	}
	var size, missing int64
	for _, l := range manifest.Layers {
		size += l.Size
		if !s.hasBlob(l.Digest) {
			missing += l.Size
		}
	}
	if err := os.MkdirAll(s.blobDir(), 0o700); err != nil {
		return Pulled{}, err
	}
	if err := node.CheckSpace(s.blobDir(), missing+spaceReserve); err != nil {
		return Pulled{}, err
	}
	for _, l := range manifest.Layers {
		if s.hasBlob(l.Digest) {
			continue
		}
		layer, err := img.LayerByDigest(l.Digest)
		if err != nil {
			return Pulled{}, err
		}
		rc, err := layer.Compressed()
		if err != nil {
			return Pulled{}, err
		}
		err = s.writeBlob(l.Digest, rc)
		rc.Close()
		if err != nil {
			return Pulled{}, fmt.Errorf("layer %s: %w", l.Digest, err)
		}
	}
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return Pulled{}, err
	}
	if err := s.writeBlob(manifest.Config.Digest, bytes.NewReader(rawConfig)); err != nil {
		return Pulled{}, err
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return Pulled{}, err
	}
	// The manifest goes last: its presence marks the image as completely cached.
	if err := s.writeBlob(digest, bytes.NewReader(rawManifest)); err != nil {
		return Pulled{}, err
	}
	c := cfg.Config
	return Pulled{
		Digest:    digest.String(),
		Config:    proto.ImageConfig{Entrypoint: c.Entrypoint, Cmd: c.Cmd, Env: c.Env, WorkingDir: c.WorkingDir, StopSignal: c.StopSignal},
		SizeBytes: size,
	}, nil
}

func amd64Image(desc *remote.Descriptor) (v1.Image, error) {
	if !desc.MediaType.IsIndex() {
		return desc.Image()
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	var platforms []string
	for _, m := range im.Manifests {
		if m.Platform == nil {
			continue
		}
		if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			if m.Size > metadataLimit {
				return nil, fmt.Errorf("manifest of %d bytes exceeds %d", m.Size, metadataLimit)
			}
			return idx.Image(m.Digest)
		}
		// Attestation manifests carry unknown/unknown.
		if m.Platform.OS != "unknown" {
			platforms = append(platforms, m.Platform.String())
		}
	}
	return nil, fmt.Errorf("image platform %s is not linux/amd64", strings.Join(platforms, ", "))
}

// Prune removes cached blobs and disks not belonging to the images in keep.
func (s Store) Prune(keep []string) error {
	blobs, disks := map[string]bool{}, map[string]bool{}
	for _, d := range keep {
		h, err := parseDigest(d)
		if err != nil {
			return err
		}
		disks[h.Hex] = true
		raw, err := os.ReadFile(s.blobPath(h))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		m, err := v1.ParseManifest(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		blobs[h.Hex], blobs[m.Config.Digest.Hex] = true, true
		for _, l := range m.Layers {
			blobs[l.Digest.Hex] = true
		}
	}
	if err := removeUnkept(s.blobDir(), blobs); err != nil {
		return err
	}
	return removeUnkept(filepath.Join(s.Dir, "disks"), disks)
}

// removeUnkept deletes entries whose name up to the first dot is not in keep (covers .raw, .part, .tmp, .lock).
func removeUnkept(dir string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		key, _, _ := strings.Cut(e.Name(), ".")
		if keep[key] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func parseDigest(digest string) (v1.Hash, error) {
	h, err := v1.NewHash(digest)
	if err != nil {
		return v1.Hash{}, err
	}
	if h.Algorithm != "sha256" {
		return v1.Hash{}, fmt.Errorf("unsupported digest algorithm %s", h.Algorithm)
	}
	return h, nil
}

func (s Store) blobDir() string { return filepath.Join(s.Dir, "blobs", "sha256") }

func (s Store) blobPath(h v1.Hash) string { return filepath.Join(s.blobDir(), h.Hex) }

func (s Store) hasBlob(h v1.Hash) bool {
	_, err := os.Stat(s.blobPath(h))
	return err == nil
}

// writeBlob stores r under h only if its content hashes to h, so a partial or tampered download is never cached.
func (s Store) writeBlob(h v1.Hash, r io.Reader) error {
	if h.Algorithm != "sha256" {
		return fmt.Errorf("unsupported digest algorithm %s", h.Algorithm)
	}
	if s.hasBlob(h) {
		return nil
	}
	f, err := os.CreateTemp(s.blobDir(), h.Hex+".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), r); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != h.Hex {
		return fmt.Errorf("digest mismatch: got sha256:%s", got)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.blobPath(h))
}

// image loads a pulled image from the local blobs only.
func (s Store) image(h v1.Hash) (v1.Image, error) {
	raw, err := os.ReadFile(s.blobPath(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("image %s is not pulled", h)
	}
	if err != nil {
		return nil, err
	}
	m, err := v1.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(localImage{s: s, raw: raw, m: m})
}

type localImage struct {
	s   Store
	raw []byte
	m   *v1.Manifest
}

func (li localImage) MediaType() (types.MediaType, error) { return li.m.MediaType, nil }

func (li localImage) RawManifest() ([]byte, error) { return li.raw, nil }

func (li localImage) RawConfigFile() ([]byte, error) {
	return os.ReadFile(li.s.blobPath(li.m.Config.Digest))
}

func (li localImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	for _, l := range li.m.Layers {
		if l.Digest == h {
			return localLayer{path: li.s.blobPath(h), desc: l}, nil
		}
	}
	return nil, fmt.Errorf("layer %s not in manifest", h)
}

type localLayer struct {
	path string
	desc v1.Descriptor
}

func (l localLayer) Digest() (v1.Hash, error)            { return l.desc.Digest, nil }
func (l localLayer) Size() (int64, error)                { return l.desc.Size, nil }
func (l localLayer) MediaType() (types.MediaType, error) { return l.desc.MediaType, nil }
func (l localLayer) Compressed() (io.ReadCloser, error)  { return os.Open(l.path) }
