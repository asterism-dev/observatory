package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
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

func TestEgressUsageLinesThrottled(t *testing.T) {
	out := newSyncBuf()
	report := usageReporter(&lockedWriter{w: out})
	pct := 10.0
	now := time.Now().UTC().Truncate(time.Second)
	for i := range 5 {
		p := pct + float64(i)
		report(proto.UsageObservation{Integration: "i1", Account: "acc1", FiveHourPct: &p, ObservedAt: now})
		time.Sleep(200 * time.Millisecond)
	}
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Fatalf("lines = %q", out.String())
	}
	until := now.Add(time.Hour)
	report(proto.UsageObservation{Integration: "i1", Account: "acc1", LimitedUntil: &until, ObservedAt: now})
	deadline := time.Now().Add(time.Second)
	for strings.Count(out.String(), "\n") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	for i, l := range lines {
		b, ok := strings.CutPrefix(l, "usage ")
		var o proto.UsageObservation
		if !ok || json.Unmarshal([]byte(b), &o) != nil || o.Account != "acc1" || o.Integration != "i1" {
			t.Fatalf("line %d = %q", i, l)
		}
		if i == 1 && (o.LimitedUntil == nil || !o.LimitedUntil.Equal(until)) {
			t.Fatalf("limit line = %q", l)
		}
		var fields map[string]any
		json.Unmarshal([]byte(b), &fields)
		for k := range fields {
			if !slices.Contains([]string{"integration", "account", "fiveHourPct", "limitedUntil", "observedAt"}, k) {
				t.Fatalf("unexpected field %q in %q", k, l)
			}
		}
	}
}
