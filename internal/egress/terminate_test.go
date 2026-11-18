package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	ghValue = "ghp_real_value_0123456789"
	ghPH    = "obsph_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherPH = "obsph_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func ghSecret(hosts ...string) proto.HostBoundSecret {
	return proto.HostBoundSecret{ID: "s1", Name: "GH_TOKEN", Placeholder: ghPH, Value: ghValue, Hosts: hosts}
}

type seen struct {
	host, path, query, body string
	header                  http.Header
	protoMajor              int
}

type rig struct {
	t        *testing.T
	p        *Proxy
	addr     net.Addr
	egress   proto.Egress
	vmPool   *x509.CertPool
	upPool   *x509.CertPool
	upstream string
	dials    atomic.Int32
	mu       sync.Mutex
	got      []seen
}

func (r *rig) requests() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.got...)
}

func (r *rig) last() seen {
	r.t.Helper()
	got := r.requests()
	if len(got) == 0 {
		r.t.Fatal("upstream got nothing")
	}
	return got[len(got)-1]
}

func (r *rig) record(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, seen{req.Host, req.URL.Path, req.URL.RawQuery, string(b), req.Header.Clone(), req.ProtoMajor})
	r.mu.Unlock()
	w.Write([]byte("ok"))
}

// newRig starts a TLS upstream for a.test/b.test and a proxy whose original destination is 203.0.113.9:443.
func newRig(t *testing.T, secrets []proto.HostBoundSecret, h http.Handler, opts ...func(*rig)) *rig {
	t.Helper()
	r := &rig{t: t}
	if h == nil {
		h = http.HandlerFunc(r.record)
	}
	upCA, err := NewCA([]string{"a.test", "b.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	upIss, err := NewIssuer([]proto.EgressCA{{ID: "up", CertPEM: upCA.CertPEM, KeyPEM: upCA.KeyPEM}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) { return upIss.Leaf(hi.ServerName) },
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.upstream = srv.Listener.Addr().String()
	r.upPool = x509.NewCertPool()
	r.upPool.AppendCertsFromPEM([]byte(upCA.CertPEM))

	vmCA, err := NewCA([]string{"a.test", "b.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.vmPool = x509.NewCertPool()
	r.vmPool.AppendCertsFromPEM([]byte(vmCA.CertPEM))
	r.egress = proto.Egress{CAs: []proto.EgressCA{{ID: "vm", CertPEM: vmCA.CertPEM, KeyPEM: vmCA.KeyPEM}}, Secrets: secrets}

	orig := netip.MustParseAddrPort("203.0.113.9:443")
	r.addr = startProxy(t, func(net.Conn) (netip.AddrPort, error) { return orig, nil }, func(p *Proxy, _ net.Addr) {
		r.p = p
		p.UpstreamAddr = func(string) string { return r.upstream }
		p.UpstreamRoots = r.upPool
		if err := p.SetTable(r.egress); err != nil {
			t.Fatal(err)
		}
		for _, o := range opts {
			o(r)
		}
	})
	t.Cleanup(func() { r.p.upstream().CloseIdleConnections() })
	return r
}

func (r *rig) client(h2 bool) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			r.dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, "tcp", r.addr.String())
		},
		TLSClientConfig: &tls.Config{RootCAs: r.vmPool},
	}
	var pr http.Protocols
	pr.SetHTTP1(!h2)
	pr.SetHTTP2(h2)
	tr.Protocols = &pr
	r.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func (r *rig) do(c *http.Client, req *http.Request) (*http.Response, string) {
	r.t.Helper()
	res, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return res, string(b)
}

func get(t *testing.T, url string, hdr ...string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return req
}

func TestBearerSubstitution(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	res, _ := r.do(r.client(false), get(t, "https://a.test/x", "Authorization", "Bearer "+ghPH))
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := r.last().header.Get("Authorization"); got != "Bearer "+ghValue {
		t.Fatalf("upstream saw %q", got)
	}
}

func TestBasicSubstitution(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	c := r.client(false)
	for _, tc := range [][4]string{{"x", ghPH, "x", ghValue}, {ghPH, "y", ghValue, "y"}} {
		req := get(t, "https://a.test/")
		req.SetBasicAuth(tc[0], tc[1])
		r.do(c, req)
		up := &http.Request{Header: r.last().header}
		u, pw, ok := up.BasicAuth()
		if !ok || u != tc[2] || pw != tc[3] {
			t.Fatalf("upstream saw %q:%q", u, pw)
		}
	}
}

func TestHeaderRestriction(t *testing.T) {
	s := ghSecret("a.test")
	s.Headers = []string{"authorization"}
	r := newRig(t, []proto.HostBoundSecret{s}, nil)
	r.do(r.client(false), get(t, "https://a.test/", "Authorization", "Bearer "+ghPH, "X-Token", ghPH))
	h := r.last().header
	if h.Get("Authorization") != "Bearer "+ghValue || h.Get("X-Token") != ghPH {
		t.Fatalf("authorization %q x-token %q", h.Get("Authorization"), h.Get("X-Token"))
	}
}

func TestPlaceholderNotInTableIgnored(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	r.do(r.client(false), get(t, "https://a.test/", "Authorization", "Bearer "+otherPH))
	if got := r.last().header.Get("Authorization"); got != "Bearer "+otherPH {
		t.Fatalf("upstream saw %q", got)
	}
}

func TestPlaceholderOtherHostIgnored(t *testing.T) {
	other := proto.HostBoundSecret{ID: "s2", Name: "OTHER", Placeholder: otherPH, Value: "other-value-12345", Hosts: []string{"b.test"}}
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test"), other}, nil)
	r.do(r.client(false), get(t, "https://b.test/", "Authorization", "Bearer "+ghPH))
	got := r.last()
	if got.host != "b.test" || got.header.Get("Authorization") != "Bearer "+ghPH {
		t.Fatalf("upstream saw host %q auth %q", got.host, got.header.Get("Authorization"))
	}
}

func TestHostMismatch421(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test", "b.test")}, nil)
	for _, h2 := range []bool{false, true} {
		req := get(t, "https://a.test/", "Authorization", "Bearer "+ghPH)
		req.Host = "b.test"
		res, _ := r.do(r.client(h2), req)
		if res.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("h2 %v: status %d", h2, res.StatusCode)
		}
	}
	if n := len(r.requests()); n != 0 {
		t.Fatalf("upstream got %d requests", n)
	}
}

func TestBoundIgnoresOrigDst(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	res, body := r.do(r.client(false), get(t, "https://a.test/"))
	if res.StatusCode != 200 || body != "ok" || r.last().host != "a.test" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
}

func TestNoSubstitutionOutsideHeaders(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	req, err := http.NewRequest("POST", "https://a.test/p/"+ghPH+"?q="+ghPH, strings.NewReader("body "+ghPH))
	if err != nil {
		t.Fatal(err)
	}
	r.do(r.client(false), req)
	got := r.last()
	if got.path != "/p/"+ghPH || got.query != "q="+ghPH || got.body != "body "+ghPH {
		t.Fatalf("upstream saw %+v", got)
	}
}

func TestRequestRewrites(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	r.do(r.client(false), get(t, "https://a.test/",
		"Accept-Encoding", "gzip, br", "Sec-WebSocket-Extensions", "permessage-deflate", "X-Forwarded-For", "198.51.100.1"))
	h := r.last().header
	if h.Get("Accept-Encoding") != "identity" || h.Get("Sec-WebSocket-Extensions") != "" || h.Get("X-Forwarded-For") != "" {
		t.Fatalf("upstream headers %v", h)
	}
}

func TestUpstreamBadCert502(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil, func(r *rig) { r.p.UpstreamRoots = x509.NewCertPool() })
	res, body := r.do(r.client(false), get(t, "https://a.test/"))
	if res.StatusCode != http.StatusBadGateway || body != "egress: certificate" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
}

func TestUpstreamForbidden502(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil, func(r *rig) { r.p.Policy = DefaultPolicy() })
	res, body := r.do(r.client(false), get(t, "https://a.test/"))
	if res.StatusCode != http.StatusBadGateway || body != "egress: forbidden" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	if n := len(r.requests()); n != 0 {
		t.Fatalf("upstream got %d requests", n)
	}
}

func TestTableSwapNextRequest(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
		c := r.client(h2)
		r.do(c, get(t, "https://a.test/", "Authorization", "Bearer "+ghPH))
		e := r.egress
		s := ghSecret("a.test")
		s.Value = "ghp_rotated_value_98765"
		e.Secrets = []proto.HostBoundSecret{s}
		if err := r.p.SetTable(e); err != nil {
			t.Fatal(err)
		}
		r.do(c, get(t, "https://a.test/", "Authorization", "Bearer "+ghPH))
		if got := r.last().header.Get("Authorization"); got != "Bearer "+s.Value {
			t.Fatalf("h2 %v: upstream saw %q", h2, got)
		}
		if n := r.dials.Load(); n != 1 {
			t.Fatalf("h2 %v: %d connections", h2, n)
		}
	}
}

// origListener records connections that reached the original destination, i.e. were spliced.
func origListener(t *testing.T) (func(*rig), *atomic.Int32) {
	l := listen(t)
	var n atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	ap := l.Addr().(*net.TCPAddr).AddrPort()
	return func(r *rig) { r.p.OrigDst = func(net.Conn) (netip.AddrPort, error) { return ap, nil } }, &n
}

func tlsDial(t *testing.T, addr net.Addr, cfg *tls.Config) error {
	t.Helper()
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	return tls.Client(c, cfg).Handshake()
}

func TestSetTableEmptySplices(t *testing.T) {
	opt, spliced := origListener(t)
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil, opt)
	if err := tlsDial(t, r.addr, &tls.Config{ServerName: "a.test", RootCAs: r.vmPool}); err != nil || spliced.Load() != 0 {
		t.Fatalf("bound host not terminated: %v, spliced %d", err, spliced.Load())
	}
	if err := r.p.SetTable(proto.Egress{}); err != nil {
		t.Fatal(err)
	}
	if err := tlsDial(t, r.addr, &tls.Config{ServerName: "a.test", RootCAs: r.vmPool}); err == nil || spliced.Load() != 1 {
		t.Fatalf("after empty table: err %v, spliced %d", err, spliced.Load())
	}
}

func TestBoundOtherALPNSplices(t *testing.T) {
	opt, spliced := origListener(t)
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil, opt)
	tlsDial(t, r.addr, &tls.Config{ServerName: "a.test", RootCAs: r.vmPool, NextProtos: []string{"acme-tls/1"}})
	if spliced.Load() != 1 {
		t.Fatal("bound host with foreign ALPN was not spliced")
	}
}

func TestBoundSNICaseAndDot(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	res, _ := r.do(r.client(false), get(t, "https://A.Test./", "Authorization", "Bearer "+ghPH))
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if names := res.TLS.PeerCertificates[0].DNSNames; len(names) != 1 || names[0] != "a.test" {
		t.Fatalf("leaf names %v", names)
	}
	if got := r.last().header.Get("Authorization"); got != "Bearer "+ghValue {
		t.Fatalf("upstream saw %q", got)
	}
}

func TestProtocols(t *testing.T) {
	release := make(chan struct{})
	var proto2 atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proto2.Store(int32(req.ProtoMajor))
		switch req.URL.Path {
		case "/grpc":
			w.Header().Set("Trailer", "Grpc-Status")
			w.Write([]byte("msg"))
			w.Header().Set("Grpc-Status", "0")
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: 1\n\n"))
			w.(http.Flusher).Flush()
			<-release
		}
	})
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, h)
	c := r.client(true)
	res, body := r.do(c, get(t, "https://a.test/grpc"))
	if res.ProtoMajor != 2 || proto2.Load() != 2 || body != "msg" || res.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("proto %d upstream %d body %q trailer %v", res.ProtoMajor, proto2.Load(), body, res.Trailer)
	}

	res, err := c.Do(get(t, "https://a.test/sse"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	defer close(release)
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 9)
		io.ReadFull(res.Body, b)
		got <- string(b)
	}()
	select {
	case s := <-got:
		if s != "data: 1\n\n" {
			t.Fatalf("event %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event not streamed")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLogLineNoSecrets(t *testing.T) {
	var buf syncBuffer
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil, func(r *rig) { r.p.Log = log.New(&buf, "", 0) })
	c := r.client(false)
	r.do(c, get(t, "https://a.test/repos/x/"+ghPH+"?token=querysecret42",
		"Authorization", "Bearer "+ghPH, "X-Extra", "headervalue42"))
	r.do(c, get(t, "https://a.test/repos/x/%6F"+ghPH[1:]))
	line := waitLog(t, &buf, 2)
	for _, want := range []string{"vm=vm1", "host=a.test", "method=GET", "path=/repos/x/", "status=200", "vars=GH_TOKEN"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	for _, bad := range []string{ghValue, ghPH[1:], "querysecret42", "token=", "headervalue42", "Bearer"} {
		if strings.Contains(line, bad) {
			t.Fatalf("log %q contains %q", line, bad)
		}
	}
}

func waitLog(t *testing.T, buf *syncBuffer, lines int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(buf.String(), "vars=") < lines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return buf.String()
}

// TestUpstreamErrorNoSecrets: an upstream echoing the substituted header in a malformed status line must not leak it.
func TestUpstreamErrorNoSecrets(t *testing.T) {
	ca, err := NewCA([]string{"a.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer([]proto.EgressCA{{ID: "bad", CertPEM: ca.CertPEM, KeyPEM: ca.KeyPEM}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := iss.Leaf("a.test")
	if err != nil {
		t.Fatal(err)
	}
	bad := tls.NewListener(listen(t), &tls.Config{Certificates: []tls.Certificate{*leaf}, NextProtos: []string{"http/1.1"}})
	go func() {
		for {
			c, err := bad.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				io.WriteString(c, "HTTP/1.1 "+strings.ReplaceAll(req.Header.Get("Authorization"), " ", "_")+"\r\n\r\n")
			}()
		}
	}()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	var buf syncBuffer
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil, func(r *rig) {
		r.upstream = bad.Addr().String()
		r.p.UpstreamRoots = pool
		r.p.Log = log.New(&buf, "", 0)
	})
	res, body := r.do(r.client(false), get(t, "https://a.test/", "Authorization", "Bearer "+ghPH))
	line := waitLog(t, &buf, 1)
	if res.StatusCode != http.StatusBadGateway || body != "egress: upstream" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	for _, s := range []string{body, line} {
		if strings.Contains(s, ghValue) || strings.Contains(s, ghPH) {
			t.Fatalf("leaked: %q", s)
		}
	}
}

func TestAbsoluteFormHostMismatch421(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test", "b.test")}, nil)
	c, err := net.Dial("tcp", r.addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := tls.Client(c, &tls.Config{ServerName: "a.test", RootCAs: r.vmPool, NextProtos: []string{"http/1.1"}})
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(tc, "GET https://b.test/ HTTP/1.1\r\nHost: a.test\r\nAuthorization: Bearer "+ghPH+"\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest || len(r.requests()) != 0 {
		t.Fatalf("status %d, upstream got %d", res.StatusCode, len(r.requests()))
	}
}

func TestForwardedDropped(t *testing.T) {
	r := newRig(t, []proto.HostBoundSecret{ghSecret("a.test")}, nil)
	r.do(r.client(false), get(t, "https://a.test/", "Forwarded", "for=198.51.100.1"))
	if h := r.last().header.Get("Forwarded"); h != "" {
		t.Fatalf("upstream saw Forwarded %q", h)
	}
}
