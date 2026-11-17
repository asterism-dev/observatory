package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/version"
)

const sslDir = "/var/lib/ovn/ssl"

func main() {
	hubURL := flag.String("hub", "http://127.0.0.1:8090", "hub base URL")
	nbDB := flag.String("nb", "unix:/var/run/ovn/ovnnb_db.sock", "OVN NB database")
	flag.Parse()
	log.Printf("%s %s", filepath.Base(os.Args[0]), version.Version)

	token := os.Getenv("OBSERVATORY_GATEWAY_TOKEN")
	if token == "" {
		log.Fatal("OBSERVATORY_GATEWAY_TOKEN is not set")
	}

	run := func(args ...string) (string, error) {
		if args[0] == "ovn-nbctl" {
			args = append([]string{args[0], "--db=" + *nbDB}, args[1:]...)
		}
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		return string(out), nil
	}

	dns := &dnsServer{}
	upstream := upstreamResolver()
	listen := net.JoinHostPort(netaddr.GatewayDNS.String(), "53")
	pc, err := net.ListenPacket("udp", listen)
	if err != nil {
		log.Fatal(err)
	}
	tl, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	go dns.serveUDP(pc, upstream)
	go dns.serveTCP(tl, upstream)

	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path string, body []byte) ([]byte, error) {
		return callHub(client, *hubURL, token, method, path, body)
	}

	var lastHash [32]byte
	var lastApply, lastStatus time.Time
	rebound := map[string]bool{}
	for ; ; time.Sleep(2 * time.Second) {
		if ports, err := guardBindings(run); err != nil {
			log.Print(err)
		} else {
			for _, p := range ports {
				rebound[p] = true
			}
		}
		if time.Since(lastStatus) >= 10*time.Second {
			st := observe(run)
			for p := range rebound {
				st.Rebound = append(st.Rebound, p)
			}
			slices.Sort(st.Rebound)
			if b, err := json.Marshal(st); err == nil {
				if _, err := call("POST", "/api/gateway/status", b); err != nil {
					log.Print(err)
				} else {
					lastStatus, rebound = time.Now(), map[string]bool{}
				}
			}
		}

		body, err := call("GET", "/api/gateway/state", nil)
		if err != nil {
			log.Print(err)
			continue
		}
		var st proto.GatewayState
		if err := json.Unmarshal(body, &st); err != nil {
			log.Print("decode gateway state: invalid JSON")
			continue
		}
		if err := validateState(st); err != nil {
			log.Print(err)
			continue
		}
		dns.setViews(st.Orgs, st.Devices)

		if h := sha256.Sum256(body); h != lastHash || time.Since(lastApply) > 60*time.Second {
			if err := apply(run, st, sslDir); err != nil {
				log.Print(err)
			} else {
				lastHash, lastApply = h, time.Now()
			}
		}
	}
}

// apply runs every step even if one fails; the start script's default-drop covers a failed underlay step.
func apply(run runner, st proto.GatewayState, dir string) error {
	var underlay, clients []peer
	for _, c := range st.Chassis {
		underlay = append(underlay, peer{c.PublicKey, c.IP + "/32"})
	}
	for _, d := range st.Devices {
		clients = append(clients, peer{d.PublicKey, d.IP + "/32"})
	}
	return errors.Join(
		applySB(run, st.SB, dir),
		reconcileNB(run, st),
		applyUnderlay(run, st),
		syncWG(run, "wg-underlay", st.UnderlayKey, netaddr.UnderlayPort, underlay),
		syncWG(run, "wg-clients", st.ClientsKey, netaddr.ClientsPort, clients))
}

// applySB rewrites the SB SSL files only when they change, so ovsdb-server keeps its sessions.
func applySB(run runner, sb proto.GatewaySB, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, cert, ca := filepath.Join(dir, "sb-key.pem"), filepath.Join(dir, "sb-cert.pem"), filepath.Join(dir, "sb-ca.pem")
	for _, f := range []struct {
		path, content string
		mode          os.FileMode
	}{{key, sb.Key, 0o600}, {cert, sb.Cert, 0o644}, {ca, sb.CACert, 0o644}} {
		if cur, err := os.ReadFile(f.path); err == nil && string(cur) == f.content {
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.content), f.mode); err != nil {
			return err
		}
	}
	// ovsdb-server reloads changed PEM files itself; rewriting the SSL or Connection rows would reset live chassis sessions.
	cur, err := run("ovn-sbctl", "get-ssl")
	if err != nil {
		return fmt.Errorf("ovn-sbctl get-ssl: %w", err)
	}
	if !sslConfigured(cur, key, cert, ca) {
		if _, err := run("ovn-sbctl", "set-ssl", key, cert, ca); err != nil {
			return fmt.Errorf("ovn-sbctl set-ssl: %w", err)
		}
	}
	remote := "pssl:6642:" + netaddr.GatewayUnderlay.String()
	if cur, err = run("ovn-sbctl", "get-connection"); err != nil {
		return fmt.Errorf("ovn-sbctl get-connection: %w", err)
	}
	if strings.TrimSpace(cur) != `read-write role="ovn-controller" `+remote {
		if _, err := run("ovn-sbctl", "set-connection", "role=ovn-controller", remote); err != nil {
			return fmt.Errorf("ovn-sbctl set-connection: %w", err)
		}
	}
	return nil
}

func sslConfigured(getSSL, key, cert, ca string) bool {
	return strings.Contains(getSSL, "Private key: "+key+"\n") &&
		strings.Contains(getSSL, "Certificate: "+cert+"\n") &&
		strings.Contains(getSSL, "CA Certificate: "+ca+"\n")
}

// guardBindings clears a port binding claimed by a chassis other than the requested one; rows without a requested chassis (router and gateway ports) are left to OVN.
func guardBindings(run runner) ([]string, error) {
	out, err := run("ovn-sbctl", "--format=csv", "--data=bare", "--no-headings",
		"--columns=_uuid,logical_port,chassis,requested_chassis", "list", "port_binding")
	if err != nil {
		return nil, fmt.Errorf("ovn-sbctl list port_binding: %w", err)
	}
	var rebound []string
	var errs []error
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 4 || f[2] == "" || f[3] == "" || f[2] == f[3] {
			continue
		}
		if _, err := run("ovn-sbctl", "clear", "port_binding", f[0], "chassis"); err != nil {
			errs = append(errs, fmt.Errorf("clear binding of %s: %w", f[1], err))
			continue
		}
		log.Printf("cleared binding of port %s: claimed by chassis %s, requested %s", f[1], f[2], f[3])
		rebound = append(rebound, f[1])
	}
	return rebound, errors.Join(errs...)
}

func observe(run runner) proto.GatewayStatus {
	s := proto.GatewayStatus{Handshakes: map[string]time.Time{}, Chassis: []string{}, Rebound: []string{}, Version: version.Version}
	for _, iface := range []string{"wg-underlay", "wg-clients"} {
		hs, err := handshakes(run, iface)
		if err != nil {
			log.Print(err)
		}
		for k, v := range hs {
			s.Handshakes[k] = v
		}
	}
	if out, err := run("ovn-sbctl", "--bare", "--columns=name", "find", "chassis"); err == nil {
		s.Chassis = strings.Fields(out)
	} else {
		log.Print(err)
	}
	return s
}

func upstreamResolver() string {
	b, _ := os.ReadFile("/etc/resolv.conf")
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
			return net.JoinHostPort(f[1], "53")
		}
	}
	return "127.0.0.11:53"
}

func validateState(st proto.GatewayState) error {
	if st.UnderlayKey == "" || st.ClientsKey == "" || st.GuestMTU <= 0 {
		return errors.New("incomplete gateway state (missing keys or guest MTU)")
	}
	if st.SB.CACert == "" || st.SB.Cert == "" || st.SB.Key == "" {
		return errors.New("incomplete gateway state (missing southbound certificate)")
	}
	return nil
}

func callHub(client *http.Client, baseURL, token, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return b, nil
}
