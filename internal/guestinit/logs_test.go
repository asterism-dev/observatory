package guestinit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

func TestFollowCmdLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmd.log")
	if err := os.WriteFile(path, []byte("before boot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	lines := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go followFile(ctx, path, int64(len("before boot\n")), 5*time.Millisecond, func(l []string) {
		mu.Lock()
		got = append(got, l...)
		mu.Unlock()
	})

	appendTo := func(p, s string) {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s)
		f.Close()
	}
	appendTo(path, "a\nb")
	waitFor(t, func() bool { return len(lines()) == 1 })
	appendTo(path, "\nc\n")
	waitFor(t, func() bool { return len(lines()) == 3 })
	// Appended right before the rename: the follower may only find it by draining the old file.
	appendTo(path, "d\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendTo(path, "e1\ne2\n")
	waitFor(t, func() bool { return len(lines()) == 6 })
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(path, "f\n")
	waitFor(t, func() bool { return len(lines()) == 7 })
	appendTo(path, strings.Repeat("x", maxLogLine+10)+"\ng\n")
	waitFor(t, func() bool { return len(lines()) == 9 })
	time.Sleep(50 * time.Millisecond)

	got = lines()
	if want := []string{"a", "b", "c", "d", "e1", "e2", "f", strings.Repeat("x", maxLogLine), "g"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %q", got)
	}
}

func TestJournalEntryToRecord(t *testing.T) {
	rec, cursor, ok := journalRecord([]byte(`{"__CURSOR":"s=1","__REALTIME_TIMESTAMP":"1700000000000001","MESSAGE":"started","PRIORITY":"4","_SYSTEMD_UNIT":"nginx.service","_TRANSPORT":"stdout"}`))
	if !ok || cursor != "s=1" {
		t.Fatalf("ok=%v cursor=%q", ok, cursor)
	}
	if rec.GetBody().GetStringValue() != "started" || rec.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_WARN || rec.TimeUnixNano != 1700000000000001000 {
		t.Fatalf("record = %v", rec)
	}
	if attrs := attrString(rec.Attributes); attrs != "unit=nginx.service" {
		t.Fatalf("attrs = %s", attrs)
	}
	for prio, want := range map[string]logspb.SeverityNumber{"0": logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "3": logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "5": logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "6": logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "7": logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG} {
		rec, _, _ := journalRecord([]byte(`{"MESSAGE":"m","PRIORITY":"` + prio + `"}`))
		if rec.SeverityNumber != want {
			t.Fatalf("priority %s = %v, want %v", prio, rec.SeverityNumber, want)
		}
	}

	rec, cursor, ok = journalRecord([]byte(`{"__CURSOR":"s=2","MESSAGE":"oops","_TRANSPORT":"kernel"}`))
	if ok || rec != nil || cursor != "s=2" {
		t.Fatalf("kernel entry: rec=%v ok=%v cursor=%q", rec, ok, cursor)
	}

	rec, _, ok = journalRecord([]byte(`{"MESSAGE":"` + strings.Repeat("y", 20<<10) + `"}`))
	if !ok || len(rec.GetBody().GetStringValue()) != maxLogLine {
		t.Fatalf("long message: ok=%v len=%d", ok, len(rec.GetBody().GetStringValue()))
	}

	rec, _, ok = journalRecord([]byte(`{"MESSAGE":[104,105]}`))
	if !ok || rec.GetBody().GetStringValue() != "hi" {
		t.Fatalf("binary message = %v", rec)
	}
}

func TestFollowJournalStartsInImage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "journalctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + filepath.Join(dir, "args") + "\necho '{\"__CURSOR\":\"c1\",\"MESSAGE\":\"hi\"}'\nsleep 5\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var started atomic.Bool
	got := make(chan string, 1)
	go followJournal(ctx, bin, func(cmd *exec.Cmd) error {
		started.Store(true)
		return cmd.Start()
	}, func(recs []*logspb.LogRecord) { got <- recs[0].GetBody().GetStringValue() })
	select {
	case msg := <-got:
		if msg != "hi" || !started.Load() {
			t.Fatalf("msg %q, started through the image %v", msg, started.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no entry")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	// Without --all, journalctl writes fields over 4 096 bytes as null.
	if !slices.Contains(strings.Fields(string(args)), "--all") {
		t.Fatalf("journalctl args = %q", args)
	}
}
