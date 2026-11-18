package egress

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"
)

// scrubIdle is how long a possible start of a value is held back before it is flushed.
var scrubIdle = 50 * time.Millisecond

// idleRawMax is the longest held-back prefix flushed unmasked; longer ones are flushed as '*'.
const idleRawMax = 3

type chunk struct {
	b   []byte
	err error
}

// scrubReader overwrites every pattern match with '*' in place, so the stream keeps its length.
type scrubReader struct {
	r        io.Reader
	patterns [][]byte
	maxLen   int
	idle     time.Duration
	buf      []byte // held source bytes, original content; the first `emitted` of them were already flushed
	hit      []bool // parallel to buf: byte belongs to a match
	emitted  int
	out      []byte
	err      error
	atEOF    func()

	// A pump goroutine reads ahead so Read can flush held bytes when the source goes idle.
	ch       chan chunk
	started  sync.Once
	stopOnce sync.Once
	done     chan struct{}
	exited   chan struct{}
}

func newScrubReader(r io.Reader, patterns [][]byte) *scrubReader {
	s := &scrubReader{r: r, patterns: patterns, idle: scrubIdle, ch: make(chan chunk), done: make(chan struct{}), exited: make(chan struct{})}
	for _, p := range patterns {
		s.maxLen = max(s.maxLen, len(p))
	}
	return s
}

func (s *scrubReader) pump() {
	defer close(s.exited)
	b := make([]byte, 32<<10)
	for {
		n, err := s.r.Read(b)
		select {
		case s.ch <- chunk{bytes.Clone(b[:n]), err}:
		case <-s.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// Close stops the pump; close the source first so a blocked source Read returns.
func (s *scrubReader) Close() error {
	s.stopOnce.Do(func() { close(s.done) })
	s.started.Do(func() { close(s.exited) })
	<-s.exited
	return nil
}

func (s *scrubReader) Read(p []byte) (int, error) {
	s.started.Do(func() { go s.pump() })
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		var idle <-chan time.Time
		var t *time.Timer
		if len(s.buf) > s.emitted {
			t = time.NewTimer(s.idle)
			idle = t.C
		}
		select {
		case c := <-s.ch:
			s.buf = append(s.buf, c.b...)
			s.hit = append(s.hit, make([]bool, len(c.b))...)
			if c.err != nil {
				s.err = c.err
				if c.err == io.EOF && s.atEOF != nil {
					s.atEOF()
				}
			}
			s.scan()
		case <-idle:
			s.flushIdle()
		case <-s.done:
			return 0, io.ErrClosedPipe
		}
		if t != nil {
			t.Stop()
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// scan emits everything except the longest suffix that could still grow into a match.
func (s *scrubReader) scan() {
	markMatches(s.buf, s.patterns, s.hit)
	keep := 0
	if s.err != io.EOF {
		keep = s.pendingSuffix()
	}
	// An aborted stream must not reveal the start of a value it never finished.
	if s.err != nil && s.err != io.EOF {
		for i := len(s.hit) - keep; i < len(s.hit); i++ {
			s.hit[i] = true
		}
		keep = 0
	}
	cut := len(s.buf) - keep
	s.emitTo(max(cut, s.emitted))
	s.buf = append(s.buf[:0], s.buf[cut:]...)
	s.hit = append(s.hit[:0], s.hit[cut:]...)
	s.emitted -= cut
}

// flushIdle releases the held tail but keeps it for matching, so a value completed later is still masked after its first bytes.
func (s *scrubReader) flushIdle() {
	if len(s.buf) > idleRawMax {
		for i := s.emitted; i < len(s.buf); i++ {
			s.hit[i] = true
		}
	}
	s.emitTo(len(s.buf))
}

func (s *scrubReader) emitTo(end int) {
	s.out = make([]byte, end-s.emitted)
	for i := range s.out {
		s.out[i] = s.buf[s.emitted+i]
		if s.hit[s.emitted+i] {
			s.out[i] = '*'
		}
	}
	s.emitted = end
}

func (s *scrubReader) pendingSuffix() int {
	for i := max(0, len(s.buf)-s.maxLen+1); i < len(s.buf); i++ {
		tail := s.buf[i:]
		for _, p := range s.patterns {
			if len(p) > len(tail) && bytes.HasPrefix(p, tail) {
				return len(tail)
			}
		}
	}
	return 0
}

func markMatches(b []byte, patterns [][]byte, hit []bool) {
	for _, p := range patterns {
		for i := 0; ; {
			j := bytes.Index(b[i:], p)
			if j < 0 {
				break
			}
			for k := i + j; k < i+j+len(p); k++ {
				hit[k] = true
			}
			i += j + 1
		}
	}
}

// scrubHeader masks values, drops names containing a pattern and Alt-Svc.
func scrubHeader(h map[string][]string, patterns [][]byte) {
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.EqualFold(k, "Alt-Svc") || slices.ContainsFunc(patterns, func(p []byte) bool { return strings.Contains(lk, strings.ToLower(string(p))) }) {
			delete(h, k)
			continue
		}
		for i := range vs {
			vs[i] = scrubString(vs[i], patterns)
		}
	}
}

func scrubString(s string, patterns [][]byte) string {
	b := []byte(s)
	hit := make([]bool, len(b))
	markMatches(b, patterns, hit)
	for i, h := range hit {
		if h {
			b[i] = '*'
		}
	}
	return string(b)
}

// scrubPatterns drops empty patterns (they would match everywhere) and duplicates.
func scrubPatterns(values [][]byte, sent []string) [][]byte {
	var out [][]byte
	add := func(p []byte) {
		if len(p) > 0 && !slices.ContainsFunc(out, func(o []byte) bool { return bytes.Equal(o, p) }) {
			out = append(out, p)
		}
	}
	for _, v := range values {
		add(v)
	}
	for _, v := range sent {
		add([]byte(v))
	}
	return out
}

type unsupportedEncodingError string

func (e unsupportedEncodingError) Error() string { return "unsupported content-encoding " + string(e) }

// encodingName keeps raw upstream header bytes out of responses and logs.
func encodingName(enc string) string {
	switch enc {
	case "br", "zstd", "compress", "x-compress", "dcb", "dcz":
		return enc
	}
	return "unknown"
}

type upgradeConn struct {
	*scrubReader
	rwc io.ReadWriteCloser
}

func (c upgradeConn) Write(b []byte) (int, error) { return c.rwc.Write(b) }

func (c upgradeConn) Close() error {
	err := c.rwc.Close()
	c.scrubReader.Close()
	return err
}

// scrub1xx masks informational responses, which ReverseProxy forwards without ModifyResponse.
type scrub1xx struct {
	base     http.RoundTripper
	patterns [][]byte
}

func (s scrub1xx) RoundTrip(req *http.Request) (*http.Response, error) {
	// The newest trace runs first, so this hook edits the shared map before ReverseProxy's copies it.
	trace := &httptrace.ClientTrace{Got1xxResponse: func(_ int, h textproto.MIMEHeader) error {
		scrubHeader(h, s.patterns)
		return nil
	}}
	return s.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}

type scrubBody struct {
	*scrubReader
	raw     io.Closer
	trailer func()
}

// Close scrubs trailers again: ReverseProxy reads them after Close, which may have drained the raw body.
func (b scrubBody) Close() error {
	err := b.raw.Close()
	b.scrubReader.Close()
	b.trailer()
	return err
}

func scrubResponse(res *http.Response, pats [][]byte) error {
	scrubHeader(res.Header, pats)
	scrubHeader(res.Trailer, pats)
	if res.StatusCode == http.StatusSwitchingProtocols {
		rwc, ok := res.Body.(io.ReadWriteCloser)
		if !ok {
			return errors.New("upgrade body is not writable")
		}
		// Client-to-server bytes pass through untouched; only server frames are scrubbed.
		res.Body = upgradeConn{newScrubReader(rwc, pats), rwc}
		return nil
	}
	var body io.Reader = res.Body
	decoded := false
	enc := strings.ToLower(strings.TrimSpace(strings.Join(res.Header.Values("Content-Encoding"), ",")))
	switch enc {
	case "", "identity":
	case "gzip", "x-gzip", "deflate":
		var err error
		if enc == "deflate" {
			body, err = zlib.NewReader(res.Body)
		} else {
			body, err = gzip.NewReader(res.Body)
		}
		if err == io.EOF {
			body, err = http.NoBody, nil
		}
		if err != nil {
			return err
		}
		decoded = true
		res.Header.Del("Content-Encoding")
		res.Header.Del("Content-Length")
		res.ContentLength = -1
	default:
		return unsupportedEncodingError(encodingName(enc))
	}
	raw := res.Body
	trailer := func() { scrubHeader(res.Trailer, pats) }
	sr := newScrubReader(body, pats)
	sr.atEOF = func() {
		// A decoder can stop at its own end marker; trailers only arrive once the raw body is read to EOF.
		if decoded {
			io.Copy(io.Discard, raw)
		}
		trailer()
	}
	res.Body = scrubBody{sr, raw, trailer}
	return nil
}
