package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/version"
)

type Hub struct {
	URL, Token string
	HTTP       *http.Client
}

func (h Hub) call(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return b, err
}

func (h Hub) State(ctx context.Context) (proto.StorageState, error) {
	var st proto.StorageState
	b, err := h.call(ctx, http.MethodGet, "/api/storage/state", nil)
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	return st, err
}

func (h Hub) Report(ctx context.Context, st proto.StorageStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = h.call(ctx, http.MethodPost, "/api/storage/status", bytes.NewReader(b))
	return err
}

// Run polls the hub and applies its state; without a state from the hub it changes nothing.
func Run(ctx context.Context, hub Hub, dir Dir, ex *Exports, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	rs := &restores{running: map[string]bool{}, done: map[string]bool{}}
	for {
		tick(ctx, hub, dir, ex, rs)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func tick(ctx context.Context, hub Hub, dir Dir, ex *Exports, rs *restores) {
	state, err := hub.State(ctx)
	if err != nil {
		log.Printf("hub state: %v", err)
		return
	}
	if err := dir.Apply(state.Disks, state.Deleted); err != nil {
		log.Printf("disks: %v", err)
	}
	rs.step(dir, ex, state.Disks)
	status := proto.StorageStatus{Exports: ex.Apply(state.Exports), Version: version.Version}
	status.Restored = rs.tokens()
	if status.Files, err = dir.Scan(); err != nil {
		log.Printf("scan: %v", err)
		return
	}
	if err := hub.Report(ctx, status); err != nil {
		log.Printf("hub status: %v", err)
	}
}

// restores runs disk restores in the background so a slow copy never stalls the poll loop.
type restores struct {
	mu       sync.Mutex
	running  map[string]bool // token -> in flight
	finished []proto.StorageDisk
	done     map[string]bool
}

// step restarts the exports of restores finished since the last tick and starts the pending ones.
func (r *restores) step(dir Dir, ex *Exports, disks []proto.StorageDisk) {
	r.mu.Lock()
	for _, disk := range r.finished {
		ex.Restart(disk.ID)
		r.done[disk.RestoreToken] = true
	}
	r.finished = nil
	r.mu.Unlock()
	for _, disk := range disks {
		r.mu.Lock()
		skip := disk.RestoreToken == "" || r.done[disk.RestoreToken] || r.running[disk.RestoreToken]
		if !skip {
			r.running[disk.RestoreToken] = true
		}
		r.mu.Unlock()
		if skip {
			continue
		}
		err := dir.StartRestore(disk, func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.running, disk.RestoreToken)
			if err != nil {
				log.Printf("disks: restore %s: %v", disk.ID, err)
				return
			}
			r.finished = append(r.finished, disk)
		})
		if err != nil {
			log.Printf("disks: restore %s: %v", disk.ID, err)
			r.mu.Lock()
			delete(r.running, disk.RestoreToken)
			r.mu.Unlock()
		}
	}
}

func (r *restores) tokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for t := range r.done {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
