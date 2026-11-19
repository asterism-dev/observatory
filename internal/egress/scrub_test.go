package egress

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// noIdle keeps scheduling delays from triggering idle flushes in tests that check exact output.
func noIdle(t *testing.T) {
	old := scrubIdle
	scrubIdle = time.Hour
	t.Cleanup(func() { scrubIdle = old })
}

func TestScrubAcrossChunks(t *testing.T) {
	noIdle(t)
	pats := [][]byte{[]byte("SECRETVALUE123")}
	body := "head SECRETVALUE123 mid SECRETSECRETVALUE123 tail"
	want := "head ************** mid SECRET************** tail"
	check := func(name string, r io.Reader) {
		t.Helper()
		got, err := io.ReadAll(newScrubReader(r, pats))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	for i := 0; i <= len(body); i++ {
		check("split", io.MultiReader(strings.NewReader(body[:i]), strings.NewReader(body[i:])))
	}
	check("one byte", iotest.OneByteReader(strings.NewReader(body)))
}

func TestScrubOverlapAndPrefixes(t *testing.T) {
	noIdle(t)
	pats := [][]byte{[]byte("abcab"), []byte("ab"), []byte("XYZW"), []byte("ZWQRST")}
	for _, tc := range [][2]string{
		{"abcabcab", "********"},
		{"-ab-", "-**-"},
		{"XYZW-", "****-"},
		{"XYZWQRST", "********"},
		{"ZWQRS", "ZWQRS"},
	} {
		got, _ := io.ReadAll(newScrubReader(iotest.OneByteReader(strings.NewReader(tc[0])), pats))
		if string(got) != tc[1] {
			t.Fatalf("%q: got %q want %q", tc[0], got, tc[1])
		}
		if s := scrubString(tc[0], pats); s != tc[1] {
			t.Fatalf("scrubString %q: got %q", tc[0], s)
		}
	}
}

func TestScrubDoesNotHoldBack(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte("data: 1\n\n"))
	sr := newScrubReader(pr, [][]byte{[]byte("SECRETVALUE123")})
	got := make(chan []byte, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := io.ReadAtLeast(sr, b, 9)
		got <- b[:n]
	}()
	select {
	case b := <-got:
		if !bytes.Equal(b, []byte("data: 1\n\n")) {
			t.Fatalf("got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scrub reader held back bytes")
	}
}

func TestScrubPatterns(t *testing.T) {
	got := scrubPatterns([][]byte{[]byte("a-value-12345"), nil}, []string{"", "a-value-12345", "Bearer a-value-12345"})
	if len(got) != 2 {
		t.Fatalf("patterns %q", got)
	}
}

// readWithin reads exactly n bytes or fails after d.
func readWithin(t *testing.T, r io.Reader, n int, d time.Duration) string {
	t.Helper()
	got := make(chan []byte, 1)
	go func() {
		b := make([]byte, n)
		k, _ := io.ReadFull(r, b)
		got <- b[:k]
	}()
	select {
	case b := <-got:
		return string(b)
	case <-time.After(d):
		t.Fatalf("%d bytes not delivered within %s", n, d)
		return ""
	}
}

func TestScrubIdleShortPrefixRawThenMasked(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newScrubReader(pr, [][]byte{[]byte("t13-secretvalue")})
	defer sr.Close()
	defer pw.Close()
	go io.WriteString(pw, "plainhello t")
	if got := readWithin(t, sr, 12, 300*time.Millisecond); got != "plainhello t" {
		t.Fatalf("got %q", got)
	}
	go io.WriteString(pw, "13-secretvalue tail")
	if got := readWithin(t, sr, 19, 300*time.Millisecond); got != "************** tail" {
		t.Fatalf("got %q", got)
	}
}

func TestScrubIdleLongPrefixMasked(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newScrubReader(pr, [][]byte{[]byte("t13-secretvalue")})
	defer sr.Close()
	defer pw.Close()
	go io.WriteString(pw, "x t13-se")
	if got := readWithin(t, sr, 8, 300*time.Millisecond); got != "x ******" {
		t.Fatalf("got %q", got)
	}
	go io.WriteString(pw, "cretvalue!")
	if got := readWithin(t, sr, 10, 300*time.Millisecond); got != "*********!" {
		t.Fatalf("continuation got %q", got)
	}
}

func TestScrubIdleSSEPrefix(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newScrubReader(pr, [][]byte{[]byte("sk-abcdefghijkl")})
	defer sr.Close()
	defer pw.Close()
	go io.WriteString(pw, "data: ask")
	start := time.Now()
	if got := readWithin(t, sr, 9, 300*time.Millisecond); got != "data: ask" {
		t.Fatalf("got %q", got)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("took %s", d)
	}
}

func TestScrubCloseStopsReader(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newScrubReader(pr, [][]byte{[]byte("SECRETVALUE123")})
	go io.WriteString(pw, "x")
	readWithin(t, sr, 1, 300*time.Millisecond)
	pr.Close()
	done := make(chan struct{})
	go func() { sr.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not stop the reader")
	}
	if _, err := sr.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after close succeeded")
	}
}
