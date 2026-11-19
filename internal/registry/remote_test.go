package registry

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestPublicOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"93.184.216.34:443": true, "[2606:4700::1111]:443": true,
		"127.0.0.1:443": false, "10.1.2.3:443": false, "172.16.0.1:443": false, "192.168.1.1:443": false,
		"169.254.169.254:80": false, "100.64.0.1:443": false, "0.0.0.0:443": false, "[::1]:443": false,
		"[fd00::1]:443": false, "0.1.2.3:443": false, "[64:ff9b::7f00:1]:443": false, "198.19.0.1:443": false, "255.255.255.255:443": false, "240.0.0.1:443": false, "[fe80::1]:443": false, "[::ffff:127.0.0.1]:443": false,
	} {
		if err := publicOnly("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestPublicTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/x:1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = PlatformDigest(context.Background(), ref, remote.WithTransport(PublicTransport()))
	if err == nil || !strings.Contains(err.Error(), "non-public address 127.0.0.1") {
		t.Fatalf("err %v", err)
	}
}
