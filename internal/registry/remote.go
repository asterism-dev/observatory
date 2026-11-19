package registry

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

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

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicOnly runs on every connect, after DNS and for redirects too, so a reference cannot reach the hub's own network.
func publicOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := ap.Addr().Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", ip)
	}
	return nil
}

// PublicTransport only connects to public addresses, without a proxy, for references users chose.
func PublicTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: publicOnly}).DialContext
	return t
}
