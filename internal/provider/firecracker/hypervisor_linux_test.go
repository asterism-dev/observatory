package firecracker

import (
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestFirecrackerHypervisor(t *testing.T) {
	p := &Provider{fcVersion: "Firecracker v1.18.0"}
	if got := p.Hypervisor(); got != (proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}) {
		t.Fatalf("hypervisor = %+v", got)
	}
}
