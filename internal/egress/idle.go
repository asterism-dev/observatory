package egress

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const streamIdle = 120 * time.Second

var streamIdleTimeout = streamIdle

var errStalled = errors.New("egress: upstream stream stalled")

// idleReader cancels the upstream request when no data arrives for the timeout; only that unblocks a blocked Read.
type idleReader struct {
	rc      io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	stalled atomic.Bool
	stopped atomic.Bool
}

func newIdleReader(rc io.ReadCloser, timeout time.Duration, cancel func()) *idleReader {
	r := &idleReader{rc: rc, timeout: timeout}
	r.timer = time.AfterFunc(timeout, func() {
		r.stalled.Store(true)
		cancel()
	})
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 && !r.stopped.Load() {
		r.timer.Reset(r.timeout)
	}
	if err != nil && err != io.EOF && r.stalled.Load() {
		return n, errStalled
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.stopped.Store(true)
	r.timer.Stop()
	return r.rc.Close()
}

// watchStream reports whether res is a Claude event stream the watchdog applies to.
func watchStream(host string, res *http.Response) bool {
	mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	return host == claudeHost && mt == "text/event-stream" && res.StatusCode != http.StatusSwitchingProtocols
}

// stallBody ends an aborted stream with an error event so the client does not take it for a normal end.
type stallBody struct {
	io.ReadCloser
	timeout time.Duration
	tail    io.Reader
	st      *reqStats
}

func (b *stallBody) Read(p []byte) (int, error) {
	if b.tail != nil {
		return b.tail.Read(p)
	}
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, errStalled) {
		b.st.err = "stall"
		ev := fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream sent nothing for %s, connection aborted\"}}\n\n", b.timeout)
		b.tail = strings.NewReader(ev)
		m, _ := b.tail.Read(p[n:])
		return n + m, nil
	}
	return n, err
}
