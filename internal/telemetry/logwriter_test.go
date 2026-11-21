package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

func attrs(v string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{{Key: "observatory.image", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}}
}

type fakeVL struct {
	mu     sync.Mutex
	reqs   []*collogspb.ExportLogsServiceRequest
	fields []string
}

func (f *fakeVL) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := &collogspb.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(body, req); err != nil {
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.fields = append(f.fields, r.Header.Get("VL-Stream-Fields"))
	f.mu.Unlock()
}

func TestLogWriterBatches(t *testing.T) {
	fake := &fakeVL{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, "a,b", 10000)

	for i := 0; i < 600; i++ {
		w.Write(attrs("x"), []string{"line"})
		w.Write(attrs("y"), []string{"line"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(fake.reqs))
	}
	total := 0
	for i, req := range fake.reqs {
		if fake.fields[i] != "a,b" {
			t.Fatalf("fields %q", fake.fields[i])
		}
		n := 0
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				n += len(sl.LogRecords)
			}
		}
		if n > 500 || len(req.ResourceLogs) > 2 {
			t.Fatalf("batch %d: %d lines, %d resources", i, n, len(req.ResourceLogs))
		}
		total += n
	}
	if total != 1200 {
		t.Fatalf("total = %d", total)
	}
}

func TestLogWriterBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, "a", 1000)
	go w.Run(t.Context())

	start := time.Now()
	for i := 0; i < 50000; i++ {
		w.Write(attrs("x"), []string{"l"})
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("writes took %v", d)
	}
	if n := w.Queued(); n > 1000 {
		t.Fatalf("queued = %d", n)
	}
}

func TestLogWriterDropsOldest(t *testing.T) {
	fake := &fakeVL{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, "a", 3)
	w.Write(attrs("x"), []string{"1", "2", "3", "4", "5"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var bodies []string
	for _, req := range fake.reqs {
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, r := range sl.LogRecords {
					bodies = append(bodies, r.Body.GetStringValue())
				}
			}
		}
	}
	if len(bodies) != 3 || bodies[0] != "3" || bodies[2] != "5" {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestLogWriterTruncates(t *testing.T) {
	fake := &fakeVL{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, "a", 10)
	long := make([]byte, 20<<10)
	for i := range long {
		long[i] = 'a'
	}
	w.Write(attrs("x"), []string{string(long)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.reqs) != 1 {
		t.Fatalf("requests = %d", len(fake.reqs))
	}
	rec := fake.reqs[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if n := len(rec.Body.GetStringValue()); n != 16<<10 {
		t.Fatalf("body len = %d", n)
	}
	if rec.SeverityText != "INFO" || rec.SeverityNumber != 9 || rec.TimeUnixNano == 0 {
		t.Fatalf("record %+v", rec)
	}
}

func TestLogWriterTruncatesOnRuneBoundary(t *testing.T) {
	fake := &fakeVL{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, "a", 10)
	w.Write(attrs("x"), []string{"a" + strings.Repeat("é", 10<<10)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	body := fake.reqs[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0].Body.GetStringValue()
	if !utf8.ValidString(body) || len(body) != 16<<10-1 {
		t.Fatalf("valid=%v len=%d", utf8.ValidString(body), len(body))
	}
}

func TestLogWriterFloodAllocatesLittle(t *testing.T) {
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: "http://127.0.0.1:1"}, "a", 1000)
	lines := make([]string, 1<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	w.Write(attrs("x"), lines)
	runtime.ReadMemStats(&after)
	if n := w.Queued(); n != 1000 {
		t.Fatalf("queued = %d", n)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 2<<20 {
		t.Fatalf("allocated %d bytes", d)
	}
}

func TestLogWriterKeepsOrderWithinWrite(t *testing.T) {
	fake := &fakeVL{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, "a", 10)
	w.Write(attrs("x"), []string{"1", "2", "3"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	recs := fake.reqs[0].ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 3 || recs[0].TimeUnixNano >= recs[1].TimeUnixNano || recs[1].TimeUnixNano >= recs[2].TimeUnixNano {
		t.Fatalf("timestamps %v", recs)
	}
}
