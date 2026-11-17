package linuxvm

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func parseNBDURL(raw string) (host, port, export string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	host, port, err = net.SplitHostPort(u.Host)
	export = strings.TrimPrefix(u.Path, "/")
	if u.Scheme != "nbd" || err != nil || export == "" {
		return "", "", "", fmt.Errorf("invalid NBD URL %q, want nbd://host:port/export", raw)
	}
	return host, port, export, nil
}

// parseNBDRecord reads a device record: the device, then the NBD URL it serves (absent in records of older nodes).
func parseNBDRecord(b []byte) (dev, url string) {
	dev, url, _ = strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(dev), strings.TrimSpace(url)
}

const nbdReconnectSeconds = 7 * 24 * 60 * 60

var validPSK = regexp.MustCompile(`^[0-9a-f]{64}$`)

func nbdKeyDir(vmDir, diskID string) string { return filepath.Join(vmDir, "nbd-"+diskID+"-tls") }

// writeNBDKey stores the export key where qemu-nbd's tls-creds-psk object reads it, so it never appears in process arguments.
func writeNBDKey(vmDir, diskID, psk string) (string, error) {
	if !validPSK.MatchString(psk) {
		return "", fmt.Errorf("disk %s: invalid NBD key, want 64 lowercase hex characters", diskID)
	}
	dir := nbdKeyDir(vmDir, diskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, "keys.psk"), []byte(diskID+":"+psk+"\n"), 0o600)
}

// qemuNBDArgs connects dev to the export through qemu-nbd over TLS-PSK; its NBD driver holds requests while the server is away and resumes them after reconnecting.
func qemuNBDArgs(dev, host, port, export, tlsDir string) []string {
	esc := func(s string) string { return strings.ReplaceAll(s, ",", ",,") }
	return []string{"--connect=" + dev,
		"--object", fmt.Sprintf("tls-creds-psk,id=tls0,endpoint=client,dir=%s,username=%s", esc(tlsDir), esc(export)),
		"--image-opts", fmt.Sprintf(
			"driver=raw,file.driver=nbd,file.server.type=inet,file.server.host=%s,file.server.port=%s,file.export=%s,file.reconnect-delay=%d,file.tls-creds=tls0",
			esc(host), esc(port), esc(export), nbdReconnectSeconds)}
}
