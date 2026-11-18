package egress

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func startProxy(t *testing.T, dst func(net.Conn) (netip.AddrPort, error), opts ...func(p *Proxy, self net.Addr)) net.Addr {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	p := New("vm1", log.New(io.Discard, "", 0))
	p.Policy = Policy{}
	p.OrigDst = dst
	for _, o := range opts {
		o(p, l.Addr())
	}
	go p.Serve(l)
	return l.Addr()
}

func fixedDst(a net.Addr) func(net.Conn) (netip.AddrPort, error) {
	ap := a.(*net.TCPAddr).AddrPort()
	return func(net.Conn) (netip.AddrPort, error) { return ap, nil }
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func expectClosed(t *testing.T, c net.Conn, within time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(within))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("connection still open")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("not closed within %v", within)
	}
}

// clientHello returns the first TLS record a client writes for sni.
func clientHello(t *testing.T, sni string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	go tls.Client(a, &tls.Config{ServerName: sni}).Handshake()
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(b, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}
	b.Close()
	return append(hdr, body...)
}

type writeRecorder struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()
	return w.Conn.Write(p)
}

func TestSpliceUnboundReplaysHello(t *testing.T) {
	ca, err := NewCA([]string{"example.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer([]proto.EgressCA{{ID: "c1", CertPEM: ca.CertPEM, KeyPEM: ca.KeyPEM}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := iss.Leaf("example.test")
	if err != nil {
		t.Fatal(err)
	}
	backend := tls.NewListener(listen(t), &tls.Config{Certificates: []tls.Certificate{*leaf}})
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(io.Discard, c)
			}()
		}
	}()

	up := listen(t)
	var recorded bytes.Buffer
	forwarded := make(chan struct{})
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, err := net.Dial("tcp", backend.Addr().String())
		if err != nil {
			return
		}
		defer b.Close()
		go io.Copy(c, b)
		io.Copy(b, io.TeeReader(c, &recorded))
		close(forwarded)
	}()

	addr := startProxy(t, fixedDst(up.Addr()))
	raw, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	rec := &writeRecorder{Conn: raw}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	tc := tls.Client(rec, &tls.Config{ServerName: "example.test", RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tc.ConnectionState().PeerCertificates[0].Raw, leaf.Certificate[0]) {
		t.Fatal("client did not see the upstream leaf")
	}
	tc.Close()
	select {
	case <-forwarded:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not see EOF")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !bytes.Equal(recorded.Bytes(), rec.buf.Bytes()) {
		t.Fatalf("upstream got %d bytes, client wrote %d", recorded.Len(), rec.buf.Len())
	}
}

func TestPeekClientHello(t *testing.T) {
	hello := clientHello(t, "Example.Test")
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go a.Write(hello)
	raw, info, err := peekClientHello(b)
	if err != nil || info == nil {
		t.Fatalf("hello %v err %v", info, err)
	}
	if NormalizeSNI(info.ServerName) != "example.test" || !bytes.Equal(raw, hello) {
		t.Fatalf("sni %q, raw equal %v", info.ServerName, bytes.Equal(raw, hello))
	}
}

func TestSpliceSplitClientHello(t *testing.T) {
	hello := clientHello(t, "example.test")
	up := listen(t)
	got := make(chan []byte, 1)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
	}()
	addr := startProxy(t, fixedDst(up.Addr()))
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	third := len(hello) / 3
	for _, seg := range [][]byte{hello[:third], hello[third : 2*third], hello[2*third:]} {
		c.Write(seg)
		time.Sleep(50 * time.Millisecond)
	}
	c.(*net.TCPConn).CloseWrite()
	select {
	case b := <-got:
		if !bytes.Equal(b, hello) {
			t.Fatalf("upstream got %d bytes, want %d identical", len(b), len(hello))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream got nothing")
	}
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d > %d", runtime.NumGoroutine(), base)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSpliceNonTLS(t *testing.T) {
	up := listen(t)
	got := make(chan []byte, 1)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c) // needs the client's half-close to pass through
		c.Write([]byte("ok"))
		got <- b
	}()
	addr := startProxy(t, fixedDst(up.Addr()))
	base := runtime.NumGoroutine()
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("SSH-2.0-x\r\n"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply, err := io.ReadAll(c)
	if err != nil || string(reply) != "ok" {
		t.Fatalf("reply %q err %v", reply, err)
	}
	if b := <-got; string(b) != "SSH-2.0-x\r\n" {
		t.Fatalf("upstream got %q", b)
	}
	c.Close()
	waitGoroutines(t, base)
}

func TestSpliceServerSpeaksFirst(t *testing.T) {
	up := listen(t)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("220 hi"))
		io.Copy(io.Discard, c)
	}()
	addr := startProxy(t, fixedDst(up.Addr()))
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 6)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "220 hi" {
		t.Fatalf("got %q err %v", b, err)
	}
}

func TestPolicyRefusesForbidden(t *testing.T) {
	up := listen(t)
	var accepted atomic.Bool
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			accepted.Store(true)
			c.Close()
		}
	}()
	port := up.Addr().(*net.TCPAddr).Port
	pol := DefaultPolicy()
	pol.Local = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil }
	for _, ip := range []string{"127.0.0.1", "10.198.0.5", "169.254.169.254", "192.168.64.7", "10.64.0.9", "192.0.2.10", "::ffff:127.0.0.1"} {
		dst := netip.AddrPortFrom(netip.MustParseAddr(ip), uint16(port))
		addr := startProxy(t, func(net.Conn) (netip.AddrPort, error) { return dst, nil },
			func(p *Proxy, _ net.Addr) { p.Policy = pol })
		c, err := net.Dial("tcp", addr.String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("x"))
		expectClosed(t, c, 2*time.Second)
		c.Close()
		if _, err := pol.Dialer().Dial("tcp", dst.String()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: dial err %v", ip, err)
		}
	}
	if accepted.Load() {
		t.Fatal("upstream accepted a forbidden connection")
	}
	if !pol.Allowed(netip.MustParseAddr("192.0.2.11")) {
		t.Fatal("public address refused")
	}
	if pol.Allowed(netip.MustParseAddr("::ffff:10.198.0.5")) {
		t.Fatal("mapped address allowed")
	}
}

func TestDirectConnectionToProxyClosed(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		var calls atomic.Int32
		addr := startProxy(t, nil, func(p *Proxy, self net.Addr) {
			p.OrigDst = func(net.Conn) (netip.AddrPort, error) {
				calls.Add(1)
				ap := self.(*net.TCPAddr).AddrPort()
				if mapped {
					ap = netip.AddrPortFrom(netip.AddrFrom16(ap.Addr().As16()), ap.Port())
				}
				return ap, nil
			}
		})
		c, err := net.Dial("tcp", addr.String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("x"))
		expectClosed(t, c, 2*time.Second)
		c.Close()
		time.Sleep(50 * time.Millisecond)
		if n := calls.Load(); n != 1 {
			t.Fatalf("mapped %v: proxy dialed itself (%d connections)", mapped, n)
		}
	}
}

func TestPeekSplitHello(t *testing.T) {
	hello := clientHello(t, "example.test")
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		third := len(hello) / 3
		for _, seg := range [][]byte{hello[:third], hello[third : 2*third], hello[2*third:]} {
			a.Write(seg)
			time.Sleep(50 * time.Millisecond)
		}
	}()
	raw, info, err := peekClientHello(b)
	if err != nil || info == nil || !bytes.Equal(raw, hello) {
		t.Fatalf("hello %v err %v", info, err)
	}
}

func TestSpliceLargeHello(t *testing.T) {
	body := bytes.Repeat([]byte{0xab}, 20<<10)
	msg := append([]byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	var hello []byte
	for len(msg) > 0 {
		n := min(len(msg), 16<<10)
		hello = append(hello, 0x16, 0x03, 0x01, byte(n>>8), byte(n))
		hello = append(hello, msg[:n]...)
		msg = msg[n:]
	}
	up := listen(t)
	got := make(chan []byte, 1)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
	}()
	addr := startProxy(t, fixedDst(up.Addr()))
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() {
		c.Write(hello)
		c.(*net.TCPConn).CloseWrite()
	}()
	select {
	case b := <-got:
		if !bytes.Equal(b, hello) {
			t.Fatalf("upstream got %d bytes, want %d identical", len(b), len(hello))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream got nothing")
	}
}

type tempErr struct{}

func (tempErr) Error() string   { return "temporary" }
func (tempErr) Timeout() bool   { return false }
func (tempErr) Temporary() bool { return true }

type flakyListener struct {
	net.Listener
	fails atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails.Add(-1) >= 0 {
		return nil, tempErr{}
	}
	return l.Listener.Accept()
}

func TestServeRetriesTemporaryAcceptErrors(t *testing.T) {
	up := listen(t)
	go func() {
		c, err := up.Accept()
		if err == nil {
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	l := &flakyListener{Listener: listen(t)}
	l.fails.Store(3)
	p := New("vm1", log.New(io.Discard, "", 0))
	p.Policy = Policy{}
	p.OrigDst = fixedDst(up.Addr())
	go p.Serve(l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hi" {
		t.Fatalf("got %q err %v", b, err)
	}
}

func TestMaxConns(t *testing.T) {
	up := listen(t)
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	addr := startProxy(t, fixedDst(up.Addr()), func(p *Proxy, _ net.Addr) { p.MaxConns = 2 })
	open := func() net.Conn {
		c, err := net.Dial("tcp", addr.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	echo := func(c net.Conn) {
		c.Write([]byte("x"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
	}
	c1, c2 := open(), open()
	echo(c1)
	echo(c2)
	expectClosed(t, open(), 500*time.Millisecond)
	c1.Close()
	time.Sleep(100 * time.Millisecond)
	echo(open())
}
