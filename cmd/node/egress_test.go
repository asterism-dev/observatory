package main

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

type syncBuf struct {
	mu chan struct{}
	b  bytes.Buffer
}

func newSyncBuf() *syncBuf { s := &syncBuf{mu: make(chan struct{}, 1)}; return s }
func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	return s.b.String()
}

func TestRunEgressReadyAfterTable(t *testing.T) {
	pr, pw := io.Pipe()
	out := newSyncBuf()
	done := make(chan error, 1)
	go func() {
		done <- runEgress([]string{"-listen", "127.0.0.1:0", "-vm", "vm1"}, pr, out, io.Discard)
	}()
	time.Sleep(100 * time.Millisecond)
	if got := out.String(); got != "" {
		t.Fatalf("stdout before table: %q", got)
	}
	if _, err := pw.Write([]byte("{\"cas\":[],\"secrets\":[]}\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for out.String() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := out.String(); got != "ready\n" {
		t.Fatalf("stdout = %q", got)
	}
	pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no exit on stdin EOF")
	}
}

func TestRunEgressBadTable(t *testing.T) {
	line := `{"cas":`
	err := runEgress([]string{"-listen", "127.0.0.1:0"}, strings.NewReader(line+"\n"), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), line) {
		t.Fatalf("error echoes the line: %v", err)
	}
}

func TestRunEgressBadLaterTable(t *testing.T) {
	in := "{\"cas\":[],\"secrets\":[]}\n{\"cas\":\n"
	if err := runEgress([]string{"-listen", "127.0.0.1:0"}, strings.NewReader(in), io.Discard, io.Discard); err == nil {
		t.Fatal("want error")
	}
}

func TestRunEgressLineTooLong(t *testing.T) {
	in := strings.Repeat("x", maxTableLine+1) + "\n"
	err := runEgress([]string{"-listen", "127.0.0.1:0"}, strings.NewReader(in), io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), "xxxx") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunEgressBadPeer(t *testing.T) {
	err := runEgress([]string{"-listen", "127.0.0.1:0", "-peer", "nope"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-peer") {
		t.Fatalf("err = %v", err)
	}
}

func TestEgressMainDiscardsGlobalLog(t *testing.T) {
	defer log.SetOutput(log.Writer())
	var stderr bytes.Buffer
	line := `{"cas":`
	if code := egressMain([]string{"-listen", "127.0.0.1:0"}, strings.NewReader(line+"\n"), io.Discard, &stderr); code != 1 {
		t.Fatalf("exit code %d", code)
	}
	if log.Writer() != io.Discard {
		t.Fatal("global logger still writes")
	}
	if got := stderr.String(); got != "egress: invalid table\n" {
		t.Fatalf("stderr %q", got)
	}
}
