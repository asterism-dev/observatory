package buildagent

import (
	"bytes"
	"slices"
	"sync"
)

// minSecretLen keeps very short values from masking common text and destroying the log.
const minSecretLen = 4

var mask = []byte("***")

// Masker replaces secret values with *** in everything written to it; Take returns the masked text that is safe to send.
type Masker struct {
	mu      sync.Mutex
	secrets [][]byte
	raw     []byte
	out     []byte
	closed  bool
}

func NewMasker(secrets [][]byte) *Masker {
	m := &Masker{}
	for _, s := range secrets {
		if len(s) >= minSecretLen {
			m.secrets = append(m.secrets, slices.Clone(s))
		}
	}
	// Longest first, so a secret containing another is masked as a whole.
	slices.SortFunc(m.secrets, func(a, b []byte) int { return len(b) - len(a) })
	return m
}

func (m *Masker) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw = append(m.raw, p...)
	m.scan()
	return len(p), nil
}

// Close releases text held back as a possible start of a secret.
func (m *Masker) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.scan()
	return nil
}

// Take returns and clears the masked text written so far.
func (m *Masker) Take() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.out
	m.out = nil
	return out
}

// MaskString masks s on its own, apart from the streamed text.
func (m *Masker) MaskString(s string) string {
	c := &Masker{secrets: m.secrets, raw: []byte(s), closed: true}
	c.scan()
	return string(c.out)
}

// scan moves raw text to out, stopping where the rest could still become a secret with the next write.
func (m *Masker) scan() {
	i, start := 0, 0
scan:
	for i < len(m.raw) {
		rest := m.raw[i:]
		for _, s := range m.secrets {
			if bytes.HasPrefix(rest, s) {
				m.out = append(append(m.out, m.raw[start:i]...), mask...)
				i += len(s)
				start = i
				continue scan
			}
		}
		if !m.closed && m.partial(rest) {
			break
		}
		i++
	}
	m.out = append(m.out, m.raw[start:i]...)
	m.raw = append(m.raw[:0], m.raw[i:]...)
}

func (m *Masker) partial(rest []byte) bool {
	for _, s := range m.secrets {
		if len(rest) < len(s) && bytes.HasPrefix(s, rest) {
			return true
		}
	}
	return false
}
