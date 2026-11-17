package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// fcAPI talks to Firecracker's control socket, dialing it anew for each request.
type fcAPI struct{ dial func() (net.Conn, error) }

func apiAt(sock string) fcAPI {
	return fcAPI{dial: func() (net.Conn, error) { return net.Dial("unix", sock) }}
}

type netOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// balloonStats are in bytes.
type balloonStats struct {
	TotalMemory     uint64 `json:"total_memory"`
	AvailableMemory uint64 `json:"available_memory"`
}

// Snapshots write or read the whole guest memory, which takes far longer than the other calls.
const (
	apiTimeout      = 10 * time.Second
	snapshotTimeout = 10 * time.Minute
)

func (a fcAPI) Pause() error {
	return a.call(http.MethodPatch, "/vm", map[string]string{"state": "Paused"}, nil, apiTimeout)
}

func (a fcAPI) Resume() error {
	return a.call(http.MethodPatch, "/vm", map[string]string{"state": "Resumed"}, nil, apiTimeout)
}

func (a fcAPI) CtrlAltDel() error {
	return a.call(http.MethodPut, "/actions", map[string]string{"action_type": "SendCtrlAltDel"}, nil, apiTimeout)
}

func (a fcAPI) CreateSnapshot(statePath, memPath string) error {
	return a.call(http.MethodPut, "/snapshot/create", struct {
		Type  string `json:"snapshot_type"`
		State string `json:"snapshot_path"`
		Mem   string `json:"mem_file_path"`
	}{"Full", statePath, memPath}, nil, snapshotTimeout)
}

func (a fcAPI) LoadSnapshot(statePath, memPath string, nets []netOverride) error {
	return a.call(http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path":     statePath,
		"mem_backend":       map[string]string{"backend_type": "File", "backend_path": memPath},
		"resume_vm":         false,
		"network_overrides": nets,
	}, nil, snapshotTimeout)
}

func (a fcAPI) PatchDrive(id, pathOnHost string) error {
	return a.call(http.MethodPatch, "/drives/"+id, struct {
		ID   string `json:"drive_id"`
		Path string `json:"path_on_host"`
	}{id, pathOnHost}, nil, apiTimeout)
}

func (a fcAPI) BalloonStats() (balloonStats, error) {
	var st balloonStats
	return st, a.call(http.MethodGet, "/balloon/statistics", nil, &st, apiTimeout)
}

func (a fcAPI) call(method, path string, in, out any, timeout time.Duration) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return a.dial() }}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var fault struct {
			Message string `json:"fault_message"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&fault)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, fault.Message)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return nil
}
