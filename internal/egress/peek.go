package egress

import (
	"crypto/tls"
	"errors"
	"net"
	"time"
)

const (
	helloLimit   = 16 << 10
	helloTimeout = time.Second
)

var (
	errHelloLimit = errors.New("client hello too large")
	errHelloDone  = errors.New("client hello captured")
	errPeekWrite  = errors.New("peek does not write")
)

// helloReader records every byte read from the client and swallows the alert tls.Server would send.
type helloReader struct {
	net.Conn
	raw     []byte
	pending []byte
	err     error
}

func (r *helloReader) Read(p []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	left := helloLimit - len(r.raw)
	if left <= 0 {
		return 0, errHelloLimit
	}
	if len(p) > left {
		p = p[:left]
	}
	n, err := r.Conn.Read(p)
	r.raw = append(r.raw, p[:n]...)
	if err != nil && r.err == nil {
		r.err = err
	}
	return n, err
}

func (r *helloReader) Write([]byte) (int, error) { return 0, errPeekWrite }

func peekClientHello(c net.Conn) ([]byte, *tls.ClientHelloInfo, error) {
	if err := c.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return nil, nil, err
	}
	defer c.SetReadDeadline(time.Time{})
	r := &helloReader{Conn: c}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		return r.raw, nil, ignoreTimeout(err)
	}
	if r.raw[0] != 0x16 {
		return r.raw, nil, nil
	}
	r.pending = append([]byte(nil), r.raw...)
	var hello *tls.ClientHelloInfo
	_ = tls.Server(r, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		hello = h
		return nil, errHelloDone
	}}).Handshake()
	if hello != nil {
		return r.raw, hello, nil
	}
	return r.raw, nil, ignoreTimeout(r.err)
}

func ignoreTimeout(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil
	}
	return err
}
