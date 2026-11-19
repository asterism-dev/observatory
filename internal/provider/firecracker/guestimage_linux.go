package firecracker

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

const imageRetry = 5 * time.Minute

// RunGuestImages pulls the guest image and builds its root disk, retrying until both are ready, and reports its state.
func (p *Provider) RunGuestImages(ctx context.Context, report func(proto.ImageStatus)) {
	for {
		started := time.Now()
		report(proto.ImageStatus{Version: p.imageRef, State: proto.ImagePulling, StartedAt: &started})
		img, cfg, err := p.pullGuestImage(ctx)
		if err == nil {
			log.Printf("guest image %s: ready in %s", img, time.Since(started).Round(time.Second))
			ready := time.Now()
			report(proto.ImageStatus{Version: img, State: proto.ImageReady, Config: cfg, StartedAt: &started, BuiltAt: &ready})
			digest, _ := imageDigest(img)
			p.storeMu.Lock()
			p.platform = digest
			p.storeMu.Unlock()
			p.prune()
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("guest image %s: %v", p.imageRef, err)
		retry := time.Now().Add(imageRetry)
		report(proto.ImageStatus{Version: p.imageRef, State: proto.ImageFailed, Error: err.Error(), StartedAt: &started, RetryAt: &retry})
		select {
		case <-ctx.Done():
			return
		case <-time.After(imageRetry):
		}
	}
}

// pullGuestImage returns the image pinned to its digest once its root disk exists.
func (p *Provider) pullGuestImage(ctx context.Context) (string, proto.ImageConfig, error) {
	return p.pull(ctx, p.imageRef, authn.Anonymous)
}

// PullImage pulls a VM's own image and builds its root disk.
func (p *Provider) PullImage(ctx context.Context, img proto.ImageRef) (string, proto.ImageConfig, error) {
	auth := authn.Anonymous
	if img.PullToken != "" {
		auth = authn.FromConfig(authn.AuthConfig{RegistryToken: img.PullToken})
	}
	ref, cfg, err := p.pull(ctx, img.Repo+"@"+img.Digest, auth)
	if err != nil {
		return "", proto.ImageConfig{}, err
	}
	digest, _ := imageDigest(ref)
	p.prune(digest)
	return ref, cfg, nil
}

func (p *Provider) pull(ctx context.Context, ref string, auth authn.Authenticator) (string, proto.ImageConfig, error) {
	p.storeMu.RLock()
	defer p.storeMu.RUnlock()
	pulled, err := p.store.Pull(ctx, ref, auth)
	if err != nil {
		return "", proto.ImageConfig{}, err
	}
	if _, err := p.store.Disk(ctx, pulled.Digest); err != nil {
		return "", proto.ImageConfig{}, err
	}
	return pinned(ref, pulled.Digest), pulled.Config, nil
}

// prune keeps inFlight, which a VM is about to be prepared from, besides keptImages; it skips while pulls run, which a waiting writer would stall.
func (p *Provider) prune(inFlight ...string) {
	if !p.storeMu.TryLock() {
		return
	}
	defer p.storeMu.Unlock()
	keep, err := p.keptImages()
	if err == nil {
		err = p.store.Prune(append(keep, inFlight...))
	}
	if err != nil {
		log.Printf("image cache: prune: %v", err)
	}
}

// keptImages are the images pruning must keep: the platform image and those VMs still to prepare and templates use.
func (p *Provider) keptImages() ([]string, error) {
	var keep []string
	if p.platform != "" {
		keep = append(keep, p.platform)
	}
	vms, err := node.LoadVMs(p.VMsDir)
	if err != nil {
		return nil, err
	}
	for _, f := range vms {
		if !f.NeedsPrepare {
			continue
		}
		if f.Spec.Image != nil {
			keep = append(keep, f.Spec.Image.Digest)
		}
		if d, err := imageDigest(f.Image); err == nil {
			keep = append(keep, d)
		}
	}
	files, _ := filepath.Glob(filepath.Join(p.VMsDir, templatesDir, "*", templateImage))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		keep = append(keep, string(b))
	}
	return keep, nil
}
