package egress

import (
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

type Proxy struct {
	VMID     string
	Policy   Policy
	OrigDst  func(net.Conn) (netip.AddrPort, error)
	Log      *log.Logger
	MaxConns int
	// UpstreamAddr and UpstreamRoots are for tests; nil means host:443 and the system roots.
	UpstreamAddr  func(host string) string
	UpstreamRoots *x509.CertPool

	table        atomic.Pointer[table]
	active       atomic.Int64
	upstreamOnce sync.Once
	upstreamTr   *http.Transport
}

// table is immutable once stored; SetTable swaps in a new one, issuer included.
type table struct {
	bound  map[string][]proto.HostBoundSecret
	issuer *Issuer
	scrub  [][]byte
}

func New(vmID string, l *log.Logger) *Proxy {
	p := &Proxy{VMID: vmID, Policy: DefaultPolicy(), OrigDst: OriginalDst, Log: l, MaxConns: 1024}
	p.table.Store(&table{})
	return p
}

func (p *Proxy) SetTable(e proto.Egress) error {
	iss, err := NewIssuer(e.CAs, time.Now)
	if err != nil {
		return err
	}
	t := &table{bound: map[string][]proto.HostBoundSecret{}, issuer: iss}
	for _, s := range e.Secrets {
		for _, h := range s.Hosts {
			h = NormalizeSNI(h)
			t.bound[h] = append(t.bound[h], s)
		}
		t.scrub = append(t.scrub, []byte(s.Value))
	}
	p.table.Store(t)
	return nil
}

func (p *Proxy) Serve(l net.Listener) error {
	var delay time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Temporary() {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		if p.active.Add(1) > int64(p.MaxConns) {
			p.active.Add(-1)
			c.Close()
			continue
		}
		go func() {
			defer p.active.Add(-1)
			p.handle(c)
		}()
	}
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	dst, err := p.OrigDst(c)
	if err != nil {
		p.Log.Printf("egress vm %s: original destination: %v", p.VMID, err)
		return
	}
	if self, ok := c.LocalAddr().(*net.TCPAddr); ok && unmap(self.AddrPort()) == unmap(dst) {
		return
	}
	raw, hello, err := peekClientHello(c)
	if err != nil {
		return
	}
	sni := ""
	if hello != nil {
		sni = NormalizeSNI(hello.ServerName)
	}
	if hello != nil && len(p.table.Load().bound[sni]) > 0 && httpALPN(hello.SupportedProtos) {
		p.terminate(c, raw, sni)
		return
	}
	p.splice(c, raw, dst, sni)
}

func unmap(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}

// httpALPN accepts no ALPN or an offer we can negotiate; anything else (acme-tls/1, ...) is not ours to terminate.
func httpALPN(protos []string) bool {
	return len(protos) == 0 || slices.Contains(protos, "h2") || slices.Contains(protos, "http/1.1")
}

func (p *Proxy) splice(c net.Conn, raw []byte, dst netip.AddrPort, sni string) {
	up, err := p.Policy.Dialer().Dial("tcp", dst.String())
	if err != nil {
		p.Log.Printf("egress vm %s sni %q dst %s: %v", p.VMID, sni, dst, err)
		return
	}
	defer up.Close()
	if len(raw) > 0 {
		if _, err := up.Write(raw); err != nil {
			return
		}
	}
	done := make(chan error, 2)
	go func() { done <- copyHalf(up, c) }()
	go func() { done <- copyHalf(c, up) }()
	if err := <-done; err != nil {
		c.Close()
		up.Close()
	}
	<-done
}

// copyHalf forwards EOF as a half-close so protocols that wait for it keep working.
func copyHalf(dst, src net.Conn) error {
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return dst.Close()
}
