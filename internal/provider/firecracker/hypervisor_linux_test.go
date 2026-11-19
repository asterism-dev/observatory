package firecracker

import (
	"errors"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestFirecrackerHypervisor(t *testing.T) {
	p := &Provider{fcVersion: "Firecracker v1.18.0"}
	if got := p.Hypervisor(); got != (proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}) {
		t.Fatalf("hypervisor = %+v", got)
	}
}

func TestFirecrackerHypervisorChanged(t *testing.T) {
	old := firecrackerVersionCmd
	t.Cleanup(func() { firecrackerVersionCmd = old })
	p := &Provider{fcVersion: "Firecracker v1.18.0"}
	for _, c := range []struct {
		out  string
		err  error
		want bool
	}{
		{"Firecracker v1.18.0\n\nSupported snapshot data format versions: v8.0.0\n", nil, false},
		{"Firecracker v1.19.0\n", nil, true},
		{"", errors.New("not found"), false},
	} {
		firecrackerVersionCmd = func() ([]byte, error) { return []byte(c.out), c.err }
		if got := p.HypervisorChanged(); got != c.want {
			t.Errorf("%q, %v: changed = %v, want %v", c.out, c.err, got, c.want)
		}
	}
}
