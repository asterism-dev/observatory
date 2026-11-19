//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
)

const updateUnit = "observatory-node-update"

type systemdUpdater struct{}

func newUpdater() node.Updater { return systemdUpdater{} }

// Start runs install.sh in its own transient unit, so it survives the service restart it triggers.
func (systemdUpdater) Start(ctx context.Context, hubBase string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubBase+"/node/install.sh", nil)
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
	f, err := os.CreateTemp("", "observatory-install-*.sh")
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("download install.sh: %w", err)
	}
	out, err := exec.CommandContext(ctx, "systemd-run", "--unit", updateUnit, "--collect", "sh", f.Name(), "--update", "--hub", hubBase).CombinedOutput()
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("systemd-run: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (systemdUpdater) Wait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		out, err := exec.CommandContext(ctx, "systemctl", "show", updateUnit, "-p", "ActiveState,Result").Output()
		if err != nil {
			return fmt.Errorf("systemctl show: %w", err)
		}
		props := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if s := props["ActiveState"]; s == "active" || s == "activating" {
			continue
		}
		if props["Result"] != "success" {
			return errors.New("install.sh --update failed (journalctl -u " + updateUnit + ")")
		}
		return nil
	}
}
