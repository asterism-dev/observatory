package egress

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestScrubAcrossChunks(t *testing.T) {
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
