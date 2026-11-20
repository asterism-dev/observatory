//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
)

const (
	updateUnit      = "observatory-node-update"
	downloadTimeout = 2 * time.Minute
	maxShowFailures = 5
)

type systemdUpdater struct{ dir string }

func newUpdater(dir string) node.Updater { return systemdUpdater{dir: dir} }

// Start runs install.sh in its own transient unit, so it survives the service restart it triggers.
func (u systemdUpdater) Start(ctx context.Context, hubBase string) error {
	// A unit still running from before an agent restart is the update in progress; Wait follows it.
	if props, err := unitProps(ctx); err == nil && unitActive(props) {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, hubBase+"/node/install.sh", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download install.sh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download install.sh: %s", resp.Status)
	}
	script := filepath.Join(u.dir, "update.sh")
	f, err := os.OpenFile(script, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download install.sh: %w", err)
	}
	// Without --collect a failed unit keeps its Result for Wait, but then blocks the unit name until reset.
	exec.CommandContext(ctx, "systemctl", "reset-failed", updateUnit).Run()
	out, err := exec.CommandContext(ctx, "systemd-run", "--unit", updateUnit, "sh", script, "--update", "--hub", hubBase).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (systemdUpdater) Wait(ctx context.Context) error {
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		props, err := unitProps(ctx)
		if err != nil {
			if failures++; failures == maxShowFailures {
				return err
			}
			slog.Error("update failed", "err", err)
			continue
		}
		failures = 0
		if unitActive(props) {
			continue
		}
		if props["Result"] != "success" {
			return errors.New("install.sh --update failed (journalctl -u " + updateUnit + ")")
		}
		return nil
	}
}

func unitProps(ctx context.Context) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", updateUnit, "-p", "ActiveState,Result").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl show: %w", err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	return props, nil
}

func unitActive(props map[string]string) bool {
	return props["ActiveState"] == "active" || props["ActiveState"] == "activating"
}
