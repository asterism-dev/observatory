package firecracker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
