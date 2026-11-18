package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/node"
)

func TestBaseForMissingImage(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(testImageURL))
	stock := filepath.Join(dir, hex.EncodeToString(sum[:8])+".raw")
	if err := os.WriteFile(stock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Provider{ImagesDir: dir, ImageURL: testImageURL, custom: true}
	if _, err := p.baseFor(node.VMFile{Image: "2026.11.1-deadbeef"}); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("missing guest image: err = %v", err)
	}
	if got, err := p.baseFor(node.VMFile{}); err != nil || got != stock {
		t.Fatalf("stock base = %q, %v; want %q", got, err, stock)
	}
	img := guestImagePath(dir, "2026.11.1-deadbeef")
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := p.baseFor(node.VMFile{Image: "2026.11.1-deadbeef"}); err != nil || got != img {
		t.Fatalf("guest base = %q, %v; want %q", got, err, img)
	}
}
