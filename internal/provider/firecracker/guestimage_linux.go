package firecracker

import (
	"context"
	"log"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

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
			// ponytail: keeps only the current image; a VM whose template restore fails after an upgrade cannot re-clone an older one.
			err := p.store.Prune([]string{digest})
			p.storeMu.Unlock()
			if err != nil {
				log.Printf("guest image: prune: %v", err)
			}
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
	p.storeMu.RLock()
	defer p.storeMu.RUnlock()
	pulled, err := p.store.Pull(ctx, p.imageRef, authn.Anonymous)
	if err != nil {
		return "", proto.ImageConfig{}, err
	}
	if _, err := p.store.Disk(ctx, pulled.Digest); err != nil {
		return "", proto.ImageConfig{}, err
	}
	return pinned(p.imageRef, pulled.Digest), pulled.Config, nil
}
