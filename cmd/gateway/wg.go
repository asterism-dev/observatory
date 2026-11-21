package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type peer struct {
	PublicKey string
	AllowedIP string
}

func syncWG(run runner, iface, privKey string, listenPort int, peers []peer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nListenPort = %d\n", privKey, listenPort)
	for _, p := range peers {
		fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\nAllowedIPs = %s\n", p.PublicKey, p.AllowedIP)
	}
	return withTempFile(b.String(), func(path string) error {
		if _, err := run("wg", "syncconf", iface, path); err != nil {
			return fmt.Errorf("wg syncconf %s: %w", iface, err)
		}
		return nil
	})
}

// withTempFile hands fn a private file holding content; it is removed afterwards because it may carry keys.
func withTempFile(content string, fn func(path string) error) error {
	f, err := os.CreateTemp("", "gateway-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return fn(f.Name())
}

func handshakes(run runner, iface string) (map[string]time.Time, error) {
	out, err := run("wg", "show", iface, "latest-handshakes")
	if err != nil {
		return nil, err
	}
	res := map[string]time.Time{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		var sec int64
		if _, err := fmt.Sscan(f[1], &sec); err == nil && sec > 0 {
			res[f[0]] = time.Unix(sec, 0)
		}
	}
	return res, nil
}

func endpoints(run runner, iface string) (map[string]string, error) {
	out, err := run("wg", "show", iface, "endpoints")
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] != "(none)" {
			res[f[0]] = f[1]
		}
	}
	return res, nil
}
