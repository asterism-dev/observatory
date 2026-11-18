package egress

import (
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/netip"
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

	table  atomic.Pointer[table]
	active atomic.Int64
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
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
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
	if self, ok := c.LocalAddr().(*net.TCPAddr); ok && self.AddrPort() == dst {
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
	// ponytail: bound SNIs splice too until termination (Task 8) routes them to p.terminate.
	p.splice(c, raw, dst, sni)
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
