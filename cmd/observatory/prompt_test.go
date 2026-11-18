package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type panicReader struct{ t *testing.T }

func (p panicReader) Read([]byte) (int, error) { p.t.Error("stdin read"); return 0, errors.New("no") }

func TestConfirm(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.g.yes, c.stdin = true, panicReader{t}
	if err := c.confirm("delete?"); err != nil {
		t.Fatal(err)
	}
	c.g.yes, c.terminal = false, true
	c.stdin = strings.NewReader("Y\n")
	if err := c.confirm("delete?"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "delete? [y/N] ") {
		t.Fatalf("prompt: %q", stderr)
	}
	c.stdin = strings.NewReader("\n")
	if err := c.confirm("delete?"); exitCode(err) != 1 || err.Error() != "aborted" {
		t.Fatalf("empty answer: %v", err)
	}
	c.terminal = false
	if err := c.confirm("delete?"); exitCode(err) != 2 || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("no terminal: %v", err)
	}
}

func TestConfirmWithoutTerminalMakesNoRequest(t *testing.T) {
	h := newFakeHub(t)
	c, _, _ := newTestCLI(t, h)
	fn := func(c *cli, _ []string) error {
		if err := c.confirm("delete x"); err != nil {
			return err
		}
		return c.call("DELETE", "/x", nil, nil)
	}
	if err := fn(c, nil); exitCode(err) != 2 {
		t.Fatalf("got %v", err)
	}
	if n := len(h.calls("DELETE /x")); n != 0 {
		t.Fatalf("%d DELETE requests", n)
	}
}

func TestSecretSources(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.stdin = strings.NewReader("abc\n")
	if v, err := c.secret("Value: ", ""); err != nil || v != "abc" {
		t.Fatalf("stdin: %q %v", v, err)
	}
	f := filepath.Join(t.TempDir(), "s")
	os.WriteFile(f, []byte("abc\r\n"), 0o600)
	if v, err := c.secret("Value: ", f); err != nil || v != "abc" {
		t.Fatalf("file: %q %v", v, err)
	}
	c.stdin = strings.NewReader("xyz\n")
	if v, err := c.secret("Value: ", "-"); err != nil || v != "xyz" {
		t.Fatalf("dash: %q %v", v, err)
	}
	c.terminal = true
	c.readPassword = func() ([]byte, error) { return []byte("pw"), nil }
	if v, err := c.secret("Value: ", ""); err != nil || v != "pw" {
		t.Fatalf("terminal: %q %v", v, err)
	}
	if v, err := c.secret("Value: ", "-"); err != nil || v != "pw" {
		t.Fatalf("terminal dash reads without echo: %q %v", v, err)
	}
	if !strings.Contains(stderr.String(), "Value: ") {
		t.Fatalf("prompt: %q", stderr)
	}
	c.terminal = false
	big := strings.Repeat("s3cret-dummy", 6000)
	c.stdin = strings.NewReader(big)
	_, err := c.secret("Value: ", "")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("big: %v", err)
	}
	c.stdin = strings.NewReader(strings.Repeat("a", 64<<10))
	if v, err := c.secret("Value: ", ""); err != nil || len(v) != 64<<10 {
		t.Fatalf("exactly 64 KiB: %d %v", len(v), err)
	}
	c.stdin = strings.NewReader("\n")
	if _, err := c.secret("Value: ", ""); exitCode(err) != 2 {
		t.Fatalf("empty: %v", err)
	}
	_, err = c.secret("Value: ", "/nonexistent/s3cret-dummy")
	if err == nil {
		t.Fatal("missing file accepted")
	}
}
