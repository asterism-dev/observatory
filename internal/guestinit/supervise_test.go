package guestinit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServiceRestartsWithBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock, calls := time.Unix(0, 0), 0
	var delays []time.Duration
	s := &Service{Argv: []string{"/bin/sh", "-c", "exit 1"}, Log: filepath.Join(t.TempDir(), "cmd.log")}
	s.now = func() time.Time {
		calls++
		// Run #8 (calls 17 start, 18 end) stays up for over a minute.
		if calls == 18 {
			clock = clock.Add(61 * time.Second)
		}
		return clock
	}
	s.after = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		ch := make(chan time.Time, 1)
		if len(delays) == 10 {
			cancel()
			return ch
		}
		ch <- clock
		return ch
	}
	s.Run(ctx)

	sec := time.Second
	want := []time.Duration{sec, 2 * sec, 4 * sec, 8 * sec, 16 * sec, 32 * sec, 60 * sec, 60 * sec, sec, 2 * sec}
	if !slices.Equal(delays, want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
}

func TestServiceStopSignal(t *testing.T) {
	log := filepath.Join(t.TempDir(), "cmd.log")
	s := &Service{
		Argv:       []string{"/bin/sh", "-c", `trap 'echo got-int; exit 0' INT; echo ready; while :; do sleep 0.05; done`},
		StopSignal: "SIGINT",
		Log:        log,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, func() bool { data, _ := os.ReadFile(log); return strings.Contains(string(data), "ready") })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop on SIGINT")
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "got-int") {
		t.Fatalf("log = %q", data)
	}
}

func TestServiceLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var/log/observatory/cmd.log")
	l := &rotatingLog{path: path, limit: serviceLogLimit}
	defer l.Close()
	chunk := make([]byte, 6<<20)
	for i, b := range []byte{'a', 'b', 'c'} {
		chunk[0] = b
		if _, err := l.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if len(cur) != 6<<20 || cur[0] != 'c' || len(old) != 6<<20 || old[0] != 'b' {
		t.Fatalf("cmd.log %d bytes starting %q, cmd.log.1 %d bytes starting %q", len(cur), cur[:1], len(old), old[:1])
	}
}

func TestParseSignal(t *testing.T) {
	for in, want := range map[string]string{"": "terminated", "SIGINT": "interrupt", "int": "interrupt", "9": "killed", "BOGUS": "terminated"} {
		if got := parseSignal(in).String(); got != want {
			t.Errorf("parseSignal(%q) = %s, want %s", in, got, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in 5 s")
}
