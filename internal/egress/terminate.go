package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const idleTimeout = 5 * time.Minute

// discard swallows the error logs of http.Server and ReverseProxy, which can contain URLs and request contents.
var discard = log.New(io.Discard, "", 0)

// replayConn hands the peeked ClientHello to tls.Server again and signals when the connection is closed.
type replayConn struct {
	net.Conn
	pending []byte
	once    sync.Once
	done    chan struct{}
}

func (c *replayConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

func (c *replayConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

type oneConnListener struct {
	conn      net.Conn
	accepted  sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.accepted.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func (p *Proxy) terminate(c net.Conn, raw []byte, sni string) {
	rc := &replayConn{Conn: c, pending: raw, done: make(chan struct{})}
	tc := tls.Server(rc, &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.table.Load().issuer.Leaf(sni)
		},
	})
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetHTTP2(true)
	srv := &http.Server{
		Handler:           p.boundHandler(sni),
		Protocols:         &protos,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: idleTimeout,
		ErrorLog:          discard,
	}
	l := &oneConnListener{conn: tc, closed: make(chan struct{})}
	go srv.Serve(l)
	// Hijacked (WebSocket) connections outlive the server's own tracking, so wait for the conn itself.
	<-rc.done
	l.Close()
}

type reqStats struct {
	status int
	bytes  int64
	err    string
}

type countingBody struct {
	io.ReadCloser
	n *int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	*b.n += int64(n)
	return n, err
}

func (p *Proxy) boundHandler(sni string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(ctx)
		t := p.table.Load()
		st := &reqStats{}
		var names []string
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if NormalizeSNI(host) != sni {
			fail(w, st, http.StatusMisdirectedRequest, "egress: host does not match the TLS server name")
		} else {
			var sent []string
			acct := p.newAccountTransport(r, sni, t)
			if acct != nil && acct.retry {
				names, sent = acct.sentValues(r.Header)
			} else {
				names, sent = substitute(r.Header, sni, t.bound[sni])
			}
			r.Header.Set("Accept-Encoding", "identity")
			r.Header.Del("Sec-WebSocket-Extensions")
			p.reverseProxy(sni, st, t, sent, acct, cancel).ServeHTTP(w, r)
		}
		line := "egress vm=%s host=%s method=%s path=%s status=%d bytes=%d dur=%s vars=%s"
		args := []any{p.VMID, sni, r.Method, redactPath(r.URL.Path, t), st.status, st.bytes, time.Since(start), strings.Join(names, ",")}
		if st.err != "" {
			line += " err=%s"
			args = append(args, st.err)
		}
		p.Log.Printf(line, args...)
	}
}

func fail(w http.ResponseWriter, st *reqStats, code int, msg string) {
	st.status = code
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	n, _ := io.WriteString(w, msg)
	st.bytes += int64(n)
}

// redactPath keeps the VM's own placeholders and values out of the log even when it puts them into a path; escaping stops log injection.
func redactPath(path string, t *table) string {
	for _, secrets := range t.bound {
		for _, s := range secrets {
			vs := []string{s.Placeholder, s.Value}
			for _, a := range s.Alternates {
				vs = append(vs, a.Value)
			}
			for _, v := range vs {
				if v != "" {
					path = strings.ReplaceAll(path, v, "***")
				}
			}
		}
	}
	return (&url.URL{Path: path}).EscapedPath()
}

// reverseProxy wraps acct, if not nil, around the upstream transport.
func (p *Proxy) reverseProxy(sni string, st *reqStats, t *table, sent []string, acct *accountTransport, cancel context.CancelFunc) *httputil.ReverseProxy {
	pats := scrubPatterns(t.scrub, sent)
	var tr http.RoundTripper = scrub1xx{p.upstream(), pats}
	if acct != nil {
		acct.base = tr
		tr = acct
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = sni
			pr.Out.Host = sni
			pr.Out.Header.Del("Forwarded")
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      discard,
		ModifyResponse: func(res *http.Response) error {
			st.status = res.StatusCode
			// An upgrade body must stay the raw io.ReadWriteCloser for ReverseProxy to switch protocols.
			if res.StatusCode != http.StatusSwitchingProtocols {
				res.Body = countingBody{res.Body, &st.bytes}
			}
			watch := watchStream(sni, res)
			if watch {
				res.Body = newIdleReader(res.Body, streamIdleTimeout, cancel)
			}
			if err := scrubResponse(res, pats); err != nil || !watch {
				return err
			}
			res.Body = &stallBody{ReadCloser: res.Body, timeout: streamIdleTimeout, st: st}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var encErr unsupportedEncodingError
			if errors.As(err, &encErr) {
				st.err = "encoding"
				fail(w, st, http.StatusBadGateway, "egress: upstream sent "+string(encErr)+"; cannot scrub")
				return
			}
			st.err = upstreamReason(err)
			fail(w, st, http.StatusBadGateway, "egress: "+st.err)
		},
	}
}

// upstreamReason maps errors to fixed words: transport errors can quote upstream bytes, which may echo substituted values.
func upstreamReason(err error) string {
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return "certificate"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &dnsErr), errors.As(err, &opErr) && opErr.Op == "dial":
		return "dial"
	}
	return "upstream"
}

func (p *Proxy) upstream() *http.Transport {
	p.upstreamOnce.Do(func() {
		d := p.Policy.Dialer()
		p.upstreamTr = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if p.UpstreamAddr != nil {
					host, _, _ := net.SplitHostPort(addr)
					addr = p.UpstreamAddr(host)
				}
				return d.DialContext(ctx, network, addr)
			},
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.UpstreamRoots},
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		}
	})
	return p.upstreamTr
}
