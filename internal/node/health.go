package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	healthInterval = time.Minute
	healthFailures = 3
	healthClient   = &http.Client{Timeout: 10 * time.Second}
)

// healthMonitor checks the underlay control path and falls back to the emergency channel over the public URL while it stays down.
type healthMonitor struct {
	check     func(context.Context) error
	enter     func(context.Context) // runs once per switch into emergency mode; may be nil
	emergency func(context.Context) error
	emerg     atomic.Bool
}

func (a *Agent) newHealthMonitor() *healthMonitor {
	h := &healthMonitor{check: a.checkHealth, emergency: a.emergencyPoll}
	if a.ChassisKey != "" {
		// A re-signed cert restarts the WireGuard link, which re-resolves the gateway endpoint.
		h.enter = func(ctx context.Context) {
			if err := a.refreshUnderlay(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("underlay refresh failed", "err", err)
			}
		}
	}
	return h
}

func (h *healthMonitor) inEmergency() bool { return h.emerg.Load() }

func (h *healthMonitor) run(ctx context.Context) {
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := h.check(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if h.emerg.Swap(false) {
				slog.Info("underlay control path is back, leaving emergency mode")
			}
			failures = 0
			continue
		}
		failures++
		slog.Warn("underlay health check failed", "err", err, "failures", failures)
		if failures < healthFailures {
			continue
		}
		if !h.emerg.Swap(true) {
			slog.Warn("entering emergency mode")
			if h.enter != nil {
				h.enter(ctx)
			}
		}
		if err := h.emergency(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("emergency poll failed", "err", err)
		}
	}
}

// controlURL is path on the gateway's control proxy, the only hub address reachable over the underlay.
func (a *Agent) controlURL(path string) string {
	if a.controlBase != "" {
		return a.controlBase + path
	}
	return "http://" + netip.AddrPortFrom(netaddr.GatewayUnderlay, netaddr.ControlPort).String() + path
}

func (a *Agent) checkHealth(ctx context.Context) error {
	err := a.probeHealth(ctx)
	if err != nil {
		a.healthErr.Store(err.Error())
	}
	return err
}

func (a *Agent) probeHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.controlURL("/api/nodes/health"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Credential)
	resp, err := healthClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}

// emergencyPoll reports to the hub over its public URL and runs an update it asks for; nothing else is done on this channel.
func (a *Agent) emergencyPoll(ctx context.Context) error {
	a.mu.Lock()
	updErr := a.emergencyUpdErr
	a.mu.Unlock()
	reason, _ := a.healthErr.Load().(string)
	body, err := json.Marshal(proto.EmergencyReport{Version: a.Version, Reason: clip(reason, 256), UpdateError: clip(updErr, 1024)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.HubURL+"/api/nodes/emergency", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Credential)
	req.Header.Set("Content-Type", "application/json")
	resp, err := underlayClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A report the hub refuses would be refused on every poll; dropping the update error unwedges the channel.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		a.mu.Lock()
		if a.emergencyUpdErr == updErr {
			a.emergencyUpdErr = ""
		}
		a.mu.Unlock()
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("emergency poll: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var reply proto.EmergencyReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&reply); err != nil {
		return fmt.Errorf("emergency reply: %w", err)
	}
	if reply.Update {
		a.runUpdate(ctx, func(m proto.Message) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.emergencyUpdErr = m.Update.Error
			return nil
		})
	}
	return nil
}
