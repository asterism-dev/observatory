package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

type fakeHub struct {
	mu       sync.Mutex
	polls    int
	failFrom int // polls from this one on get 503; 0 = never
	state    proto.StorageState
	statuses []proto.StorageStatus
	auth     []string
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	switch r.Method + " " + r.URL.Path {
	case "GET /api/storage/state":
		h.polls++
		if h.failFrom > 0 && h.polls >= h.failFrom {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(h.state)
	case "POST /api/storage/status":
		var st proto.StorageStatus
		json.NewDecoder(r.Body).Decode(&st)
		h.statuses = append(h.statuses, st)
	default:
		http.NotFound(w, r)
	}
}

func runTicks(t *testing.T, h *fakeHub, ex *Exports, ticks int) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			h.mu.Lock()
			n := h.polls
			h.mu.Unlock()
			if n >= ticks {
				time.Sleep(50 * time.Millisecond) // let the last tick finish
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	hub := Hub{URL: srv.URL, Token: "tok-123", HTTP: srv.Client()}
	if err := Run(ctx, hub, ex.Dir, ex, 10*time.Millisecond); err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunKeepsExportsWhenHubUnreachable(t *testing.T) {
	ex, f := newExports(t, "d1")
	h := &fakeHub{failFrom: 2, state: proto.StorageState{
		Disks:   []proto.StorageDisk{{ID: "d1", SizeBytes: 1 << 20}},
		Exports: []proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk1}},
	}}
	runTicks(t, h, ex, 4)
	if len(f.procs) != 1 || f.procs[0].stopped {
		t.Fatalf("procs %d stopped %v", len(f.procs), len(f.procs) > 0 && f.procs[0].stopped)
	}
	if fi, err := os.Stat(ex.Dir.File("d1")); err != nil || fi.Size() != 1<<20 {
		t.Fatalf("disk file changed: %v %v", fi, err)
	}
	if len(h.statuses) != 1 {
		t.Fatalf("statuses %d", len(h.statuses))
	}
}

func TestRunReports(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	ex, _ := newExports(t, "d1")
	h := &fakeHub{state: proto.StorageState{
		Disks:   []proto.StorageDisk{{ID: "d1", SizeBytes: 1 << 20}, {ID: "d2", SizeBytes: 1 << 20, Create: true}},
		Exports: []proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk1}, {DiskID: "gone", Port: 10811, PSK: psk2}},
	}}
	runTicks(t, h, ex, 2)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.statuses) == 0 {
		t.Fatal("no status")
	}
	st := h.statuses[0]
	if len(st.Files) != 2 || st.Files[0].Name != "d1.img" || st.Files[1].Name != "d2.img" || st.Version == "" {
		t.Fatalf("status %+v", st)
	}
	if len(st.Exports) != 2 || st.Exports[0].Error != "" || st.Exports[1].Error != "file missing" {
		t.Fatalf("exports %+v", st.Exports)
	}
	for _, a := range h.auth {
		if a != "Bearer tok-123" {
			t.Fatalf("auth %q", a)
		}
	}
	if strings.Contains(logs.String(), psk1) || strings.Contains(logs.String(), psk2) {
		t.Fatalf("psk in log: %s", logs.String())
	}
}

func TestRestoreOncePerToken(t *testing.T) {
	var clones int
	orig := clone
	clone = func(dst, src *os.File) error { clones++; return orig(dst, src) }
	defer func() { clone = orig }()

	ex, _ := newExports(t, "d1")
	writeDisk(t, ex.Dir.File("d1"), "AAAA", 1<<20)
	writeDisk(t, filepath.Join(ex.Dir.Path, "snap.img"), "BBBB", 1<<20)
	h := &fakeHub{state: proto.StorageState{
		Disks: []proto.StorageDisk{{ID: "d1", SizeBytes: 1 << 20, RestoreFrom: "snap.img", RestoreToken: "tok"}},
	}}
	runTicks(t, h, ex, 2)
	h.mu.Lock()
	defer h.mu.Unlock()
	if clones != 1 || len(h.statuses) < 2 {
		t.Fatalf("clones %d statuses %d", clones, len(h.statuses))
	}
	for _, st := range h.statuses {
		if !reflect.DeepEqual(st.Restored, []string{"tok"}) {
			t.Fatalf("restored %v", st.Restored)
		}
	}
}

func TestRestoreFailureNotReported(t *testing.T) {
	ex, _ := newExports(t, "d1")
	h := &fakeHub{state: proto.StorageState{
		Disks: []proto.StorageDisk{{ID: "d1", SizeBytes: 1 << 20, RestoreFrom: "snap.img", RestoreToken: "tok"}},
	}}
	runTicks(t, h, ex, 2)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, st := range h.statuses {
		if len(st.Restored) != 0 {
			t.Fatalf("restored %v", st.Restored)
		}
	}
}
