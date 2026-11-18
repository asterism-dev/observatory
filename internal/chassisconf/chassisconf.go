// Package chassisconf joins a Linux node host to the hub's WireGuard underlay and the OVN southbound DB.
package chassisconf

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const wgService = "wg-quick@wg-underlay"

type Config struct {
	Name, PrivateKey, Address, HubKey, Endpoint, SBRemote, CACert, Cert, KeyFile string
	MTU                                                                          int
}

func Apply(c Config, root string, run func(name string, args ...string) error) (changed bool, err error) {
	if c.Name == "" || c.Address == "" || c.SBRemote == "" || c.CACert == "" || c.Cert == "" || c.KeyFile == "" {
		return false, fmt.Errorf("chassis: name, address, sbRemote, caCert, cert and keyFile are required")
	}
	encapIP, _, _ := strings.Cut(c.Address, "/")

	wg := fmt.Sprintf(`[Interface]
Address = %s
PrivateKey = %s
MTU = %d

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 10.198.0.0/16
PersistentKeepalive = 25
`, c.Address, c.PrivateKey, c.MTU, c.HubKey, c.Endpoint)

	wgChanged, err := writeIfChanged(filepath.Join(root, "etc/wireguard/wg-underlay.conf"), []byte(wg), 0o600)
	if err != nil {
		return false, err
	}
	// Ubuntu's openvswitch-switch resets system-id from this file on every boot.
	idChanged, err := writeIfChanged(filepath.Join(root, "etc/openvswitch/system-id.conf"), []byte(c.Name), 0o644)
	if err != nil {
		return false, err
	}
	caPath, certPath := filepath.Join(root, "etc/ovn/ovn-ca.pem"), filepath.Join(root, "etc/ovn/ovn-chassis-cert.pem")
	caChanged, err := writeIfChanged(caPath, []byte(c.CACert), 0o644)
	if err != nil {
		return false, err
	}
	certChanged, err := writeIfChanged(certPath, []byte(c.Cert), 0o644)
	if err != nil {
		return false, err
	}
	changed = wgChanged || idChanged || caChanged || certChanged

	// A failure below must not make the retry see unchanged files and skip the restart.
	pending := filepath.Join(root, "run/observatory-chassis-restart-pending")
	if changed {
		if _, err := writeIfChanged(pending, nil, 0o644); err != nil {
			return changed, err
		}
	}
	_, statErr := os.Stat(pending)
	restart := statErr == nil

	if err := run("ovs-vsctl", "set-ssl", c.KeyFile, certPath, caPath); err != nil {
		return changed, err
	}
	if err := run("ovs-vsctl", "set", "open", ".",
		"external_ids:system-id="+c.Name,
		"external_ids:ovn-remote="+c.SBRemote,
		"external_ids:ovn-encap-type=geneve",
		"external_ids:ovn-encap-ip="+encapIP); err != nil {
		return changed, err
	}
	if !restart {
		return false, nil
	}
	if err := run("systemctl", "enable", "--now", wgService); err != nil {
		return changed, err
	}
	if err := run("systemctl", "restart", wgService, "ovn-controller"); err != nil {
		return changed, err
	}
	return true, os.Remove(pending)
}

func writeIfChanged(path string, content []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return false, err
	}
	return true, os.Chmod(path, mode)
}

func Run(name string, args ...string) error { return RunContext(context.Background())(name, args...) }

// RunContext returns a Run that kills the command once ctx ends.
func RunContext(ctx context.Context) func(name string, args ...string) error {
	return func(name string, args ...string) error {
		if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
		}
		return nil
	}
}
