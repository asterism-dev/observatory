package firecracker

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/vmtelemetry"
	"github.com/asterism-dev/observatory/internal/proto"
)

// SetTokenLookup sets the lookup every VM's token socket answers with; nil answers unavailable.
func (p *Provider) SetTokenLookup(l node.TokenLookup) { p.tokens.Store(&l) }

func (p *Provider) SetVMTelemetry(s *vmtelemetry.Sink) { p.telemetry.Store(s) }

func (p *Provider) token(vmID, kind string) (proto.IntegrationToken, string) {
	if l := p.tokens.Load(); l != nil && *l != nil {
		return (*l)(vmID, kind)
	}
	return proto.IntegrationToken{}, node.TokenUnavailable
}

// listenGuestPort binds the socket Firecracker delivers guest connections to port to, via the jail root's fd past the 107-byte limit.
func listenGuestPort(dir string, uid, port int) (net.Listener, error) {
	d, err := os.Open(jailRoot(dir))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	fd := int(d.Fd())
	name := vsockPath[1:] + "_" + strconv.Itoa(port)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: fmt.Sprintf("/proc/self/fd/%d/%s", fd, name), Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Close would unlink the name in whatever directory then has this fd number; removing the jail deletes the socket.
	l.SetUnlinkOnClose(false)
	if err := unix.Fchownat(fd, name, uid, uid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		l.Close()
		return nil, err
	}
	if err := unix.Fchmodat(fd, name, 0o600, 0); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
