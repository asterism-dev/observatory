package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	for {
		tick(ctx, hub, dir, ex)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func tick(ctx context.Context, hub Hub, dir Dir, ex *Exports) {
	state, err := hub.State(ctx)
	if err != nil {
		log.Printf("hub state: %v", err)
		return
	}
	if err := dir.Apply(state.Disks, state.Deleted); err != nil {
		log.Printf("disks: %v", err)
	}
	status := proto.StorageStatus{Exports: ex.Apply(state.Exports), Version: version.Version}
	if status.Files, err = dir.Scan(); err != nil {
		log.Printf("scan: %v", err)
		return
	}
	if err := hub.Report(ctx, status); err != nil {
		log.Printf("hub status: %v", err)
	}
}
