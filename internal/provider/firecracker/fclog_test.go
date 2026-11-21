package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/asterism-dev/observatory/internal/node/vmtelemetry"
)

func TestParseFCLogLine(t *testing.T) {
	for _, c := range []struct {
		line  string
		level slog.Level
		msg   string
	}{
		{"2026-11-20T16:41:15.719343746 [vm:fc_api:INFO] API server started.", slog.LevelInfo, "API server started."},
		{"2026-11-20T16:41:15.719463279 [vm:main:ERROR] RunWithApiError error: Failed to build MicroVM from Json: Configuration for VMM from one single json failed: Boot source error: The kernel file cannot be opened: No such file or directory (os error 2)",
			slog.LevelError, "RunWithApiError error: Failed to build MicroVM from Json: Configuration for VMM from one single json failed: Boot source error: The kernel file cannot be opened: No such file or directory (os error 2)"},
		{"2026-11-20T16:41:28.791559280 [vm:fc_api:ERROR] Received Error. Status code: 400 Bad Request. Message: Machine config error: The number of vCPUs must be greater than 0 and must be 1 or an even number if SMT is enabled.",
			slog.LevelError, "Received Error. Status code: 400 Bad Request. Message: Machine config error: The number of vCPUs must be greater than 0 and must be 1 or an even number if SMT is enabled."},
		{"2026-11-20T16:41:15.719343746 [vm:fc_vcpu 0:WARN] Received KVM_EXIT_SHUTDOWN signal", slog.LevelWarn, "Received KVM_EXIT_SHUTDOWN signal"},
		{"2026-11-20T16:41:15.719343746 [vm:main:DEBUG] x", slog.LevelDebug, "x"},
		{"thread panicked at src/main.rs:1", slog.LevelInfo, "thread panicked at src/main.rs:1"},
		{"2026-11-20T16:41:15.719343746 [vm:main:BOGUS] x", slog.LevelInfo, "2026-11-20T16:41:15.719343746 [vm:main:BOGUS] x"},
	} {
		level, msg := parseFCLogLine(c.line)
		if level != c.level || msg != c.msg {
			t.Errorf("parseFCLogLine(%q) = %v, %q; want %v, %q", c.line, level, msg, c.level, c.msg)
		}
	}
}

type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

func TestTailFCLog(t *testing.T) {
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	path := filepath.Join(t.TempDir(), fcLogName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tailFCLog(ctx, path, "vm1", "web"); close(done) }()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("2026-11-20T16:41:15.719343746 [vm:fc_api:INFO] API server started.\n")
	// The second line arrives in two writes: the tail must not emit the partial line.
	f.WriteString("2026-11-20T16:41:15.719463279 [vm:main:ERROR] Firecracker ")
	time.Sleep(1500 * time.Millisecond)
	f.WriteString("exiting with error. exit_code=1\n")
	for deadline := time.Now().Add(5 * time.Second); len(h.records()) < 2 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	recs := h.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	want := []struct {
		level slog.Level
		msg   string
	}{{slog.LevelInfo, "API server started."}, {slog.LevelError, "Firecracker exiting with error. exit_code=1"}}
	for i, r := range recs {
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if r.Level != want[i].level || r.Message != want[i].msg || attrs["source"] != "firecracker" || attrs["vm"] != "vm1" || attrs["vm_name"] != "web" {
			t.Errorf("record %d = %v %q %v", i, r.Level, r.Message, attrs)
		}
	}
}

func TestTailFCLogDrainsOnStop(t *testing.T) {
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	path := filepath.Join(t.TempDir(), fcLogName)
	if err := os.WriteFile(path, []byte("2026-11-20T16:41:15.719494064 [vm:main:ERROR] Firecracker exiting with error. exit_code=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tailFCLog(ctx, path, "vm1", "web")
	if recs := h.records(); len(recs) != 1 || recs[0].Level != slog.LevelError {
		t.Errorf("records = %v, want the last line written before the VM exited", recs)
	}
}

func TestTailLinesFromOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	old := "old boot line\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tailLines(ctx, path, int64(len(old)), func(l []string) { mu.Lock(); got = append(got, l...); mu.Unlock() })
		close(done)
	}()
	lines := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("one\ntw")
	time.Sleep(1500 * time.Millisecond)
	if l := lines(); len(l) != 1 || l[0] != "one" {
		t.Fatalf("lines = %q, want only the complete one", l)
	}
	f.WriteString("o\nlast\n")
	cancel()
	<-done
	if l := lines(); len(l) != 3 || l[1] != "two" || l[2] != "last" {
		t.Errorf("lines = %q, want one, two, last", l)
	}
}

func TestConsoleStreamEmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte("login: \r\n\r\n \t\n"+strings.Repeat("x", 20<<10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vm := uuid.New()
	type got struct {
		vm     uuid.UUID
		stream string
		recs   []*logspb.LogRecord
	}
	var emitted []got
	tailConsole(ctx, path, 0, vm, func(id uuid.UUID, stream string, recs []*logspb.LogRecord) {
		emitted = append(emitted, got{id, stream, recs})
	})
	if len(emitted) != 1 {
		t.Fatalf("emitted %d batches, want 1", len(emitted))
	}
	e := emitted[0]
	if e.vm != vm || e.stream != "console" || len(e.recs) != 2 {
		t.Fatalf("emitted = %+v", e)
	}
	for _, r := range e.recs {
		if r.SeverityText != "INFO" || r.TimeUnixNano == 0 {
			t.Errorf("record = %+v", r)
		}
	}
	if b := e.recs[0].Body.GetStringValue(); b != "login: " {
		t.Errorf("body = %q", b)
	}
	if n := len(e.recs[1].Body.GetStringValue()); n != 16<<10 {
		t.Errorf("long line body = %d bytes, want 16 KiB", n)
	}
}

func TestTailLinesBoundsLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1<<20)+"\nnext\n"+strings.Repeat("y", 100<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got []string
	tailLines(ctx, path, 0, func(l []string) { got = append(got, l...) })
	if len(got) != 3 || len(got[0]) != maxLine || got[1] != "next" || len(got[2]) != maxLine {
		var lens []int
		for _, l := range got {
			lens = append(lens, len(l))
		}
		t.Errorf("line lengths = %v, want truncated, next, truncated", lens)
	}
}

func TestTailLinesBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("line\n", 2*vmtelemetry.MaxBatch+3)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var sizes []int
	tailLines(ctx, path, 0, func(l []string) { sizes = append(sizes, len(l)) })
	if len(sizes) != 3 || sizes[0] != vmtelemetry.MaxBatch || sizes[1] != vmtelemetry.MaxBatch || sizes[2] != 3 {
		t.Errorf("batch sizes = %v", sizes)
	}
}
