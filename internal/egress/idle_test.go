package egress

import (
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const stallEvent = `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"upstream sent nothing for 100ms, connection aborted"}}` + "\n\n"

func shortIdle(t *testing.T) {
	t.Helper()
	old := streamIdleTimeout
	streamIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = old })
}

func sseHandler(contentType string, gap time.Duration, events int, hang bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", contentType)
		for i := 0; i < events; i++ {
			if i > 0 {
				time.Sleep(gap)
			}
			io.WriteString(w, "event: ping\ndata: {}\n\n")
			w.(http.Flusher).Flush()
		}
		if hang {
			<-req.Context().Done()
		}
	})
}

func watchdogGet(t *testing.T, r *rig, host string) (string, error) {
	t.Helper()
	req := get(t, "https://"+host+"/v1/messages", "Authorization", "Bearer "+claudePH)
	res, err := r.client(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func claudeRig(t *testing.T, h http.Handler, buf *syncBuffer) *rig {
	t.Helper()
	return newRig(t, []proto.HostBoundSecret{claudeSecret()}, h, func(r *rig) { r.p.Log = log.New(buf, "", 0) })
}

func TestWatchdogAbortsSilentStream(t *testing.T) {
	shortIdle(t)
	var buf syncBuffer
	r := claudeRig(t, sseHandler("text/event-stream; charset=utf-8", 0, 1, true), &buf)
	start := time.Now()
	body, err := watchdogGet(t, r, claudeHost)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if !strings.HasPrefix(body, "event: ping") || !strings.HasSuffix(body, stallEvent) {
		t.Fatalf("body %q", body)
	}
	if line := waitLog(t, &buf, 1); !strings.Contains(line, "err=stall") {
		t.Fatalf("log %q", line)
	}
}

func TestWatchdogResetByData(t *testing.T) {
	shortIdle(t)
	var buf syncBuffer
	r := claudeRig(t, sseHandler("text/event-stream", 50*time.Millisecond, 7, false), &buf)
	body, err := watchdogGet(t, r, claudeHost)
	if err != nil || strings.Contains(body, "event: error") {
		t.Fatalf("body %q err %v", body, err)
	}
}

func TestWatchdogIgnoresNonStream(t *testing.T) {
	shortIdle(t)
	var buf syncBuffer
	r := claudeRig(t, sseHandler("application/json", 250*time.Millisecond, 2, false), &buf)
	body, err := watchdogGet(t, r, claudeHost)
	if err != nil || strings.Contains(body, "event: error") || strings.Count(body, "ping") != 2 {
		t.Fatalf("body %q err %v", body, err)
	}
}

func TestWatchdogOnlyAnthropic(t *testing.T) {
	shortIdle(t)
	var buf syncBuffer
	s := ghSecret("a.test")
	r := newRig(t, []proto.HostBoundSecret{s}, sseHandler("text/event-stream", 250*time.Millisecond, 2, false), func(r *rig) { r.p.Log = log.New(&buf, "", 0) })
	req := get(t, "https://a.test/stream", "Authorization", "Bearer "+ghPH)
	_, body := r.do(r.client(false), req)
	if strings.Contains(body, "event: error") || strings.Count(body, "ping") != 2 {
		t.Fatalf("body %q", body)
	}
}
