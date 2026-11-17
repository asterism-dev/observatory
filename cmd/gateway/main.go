package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/version"
	"github.com/asterism-dev/observatory/internal/wgkey"
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		return string(out), nil
	}

	dns := newDNSServer()
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

	var reboundMu sync.Mutex
	rebound := map[string]bool{}
	go func() {
		for range time.Tick(2 * time.Second) {
			ports, err := guardBindings(run)
			if err != nil {
				log.Print(err)
			}
			if err := guardMACBindings(run); err != nil {
				log.Print(err)
			}
			reboundMu.Lock()
			for _, p := range ports {
				rebound[p] = true
			}
			reboundMu.Unlock()
		}
	}()

	var lastHash [32]byte
	var lastApply, lastStatus time.Time
	for ; ; time.Sleep(2 * time.Second) {
		if time.Since(lastStatus) >= 10*time.Second {
			st := observe(run)
			reboundMu.Lock()
			for p := range rebound {
				st.Rebound = append(st.Rebound, p)
			}
			reboundMu.Unlock()
			slices.Sort(st.Rebound)
			if b, err := json.Marshal(st); err == nil {
				if _, err := call("POST", "/api/gateway/status", b); err != nil {
					log.Print(err)
				} else {
					lastStatus = time.Now()
					reboundMu.Lock()
					for _, p := range st.Rebound {
						delete(rebound, p)
					}
					reboundMu.Unlock()
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
	st = withValidPeers(st)
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

// withValidPeers drops chassis and devices whose key or address would break wg syncconf or nft for everyone.
func withValidPeers(st proto.GatewayState) proto.GatewayState {
	valid := func(key, ip string) bool {
		a, err := netip.ParseAddr(ip)
		return wgkey.Valid(key) && err == nil && a.Is4()
	}
	var chassis []proto.GatewayChassis
	for _, c := range st.Chassis {
		if valid(c.PublicKey, c.IP) {
			chassis = append(chassis, c)
		} else {
			log.Printf("skipping chassis of node %s: invalid key or address", c.NodeID)
		}
	}
	var devices []proto.GatewayDevice
	for _, d := range st.Devices {
		if valid(d.PublicKey, d.IP) {
			devices = append(devices, d)
		} else {
			log.Printf("skipping a device of org %s: invalid key or address", d.OrgID)
		}
	}
	st.Chassis, st.Devices = chassis, devices
	return st
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
		if err := writeAtomic(f.path, f.content, f.mode); err != nil {
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

// writeAtomic renames a complete file into place so ovsdb-server never reads a partial key; the temp file starts as 0600.
func writeAtomic(path, content string, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), mode)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}

func sslConfigured(getSSL, key, cert, ca string) bool {
	return strings.Contains(getSSL, "Private key: "+key+"\n") &&
		strings.Contains(getSSL, "Certificate: "+cert+"\n") &&
		strings.Contains(getSSL, "CA Certificate: "+ca+"\n")
}

func sbRows(run runner, table, columns string) ([][]string, error) {
	out, err := run("ovn-sbctl", "--format=csv", "--data=bare", "--no-headings", "--columns="+columns, "list", table)
	if err != nil {
		return nil, fmt.Errorf("ovn-sbctl list %s: %w", table, err)
	}
	rows, err := parseCSV(out, len(strings.Split(columns, ",")))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", table, err)
	}
	return rows, nil
}

func parseCSV(out string, fields int) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = fields
	return r.ReadAll()
}

// guardBindings checks chassis names against the NB requested-chassis option (hub-gw when unset); SB copies are chassis-writable or resolved by a chassis-set hostname.
func guardBindings(run runner) ([]string, error) {
	out, err := run("ovn-nbctl", "--format=csv", "--data=bare", "--no-headings", "--columns=name,options", "list", "logical_switch_port")
	if err != nil {
		return nil, fmt.Errorf("ovn-nbctl list logical_switch_port: %w", err)
	}
	lsps, err := parseCSV(out, 2)
	if err != nil {
		return nil, fmt.Errorf("parse logical_switch_port: %w", err)
	}
	requested := map[string][]string{}
	for _, l := range lsps {
		for _, o := range strings.Fields(l[1]) {
			if v, ok := strings.CutPrefix(o, "requested-chassis="); ok {
				requested[l[0]] = strings.Split(v, ",")
			}
		}
	}
	chassis, err := sbRows(run, "chassis", "_uuid,name")
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, c := range chassis {
		names[c[0]] = c[1]
	}
	bindings, err := sbRows(run, "port_binding", "_uuid,logical_port,chassis,additional_chassis")
	if err != nil {
		return nil, err
	}
	var rebound []string
	var errs []error
	for _, f := range bindings {
		uuid, port, ch, add := f[0], f[1], f[2], f[3]
		req, ok := requested[port]
		if !ok {
			req = []string{"hub-gw"}
		}
		var cols []string
		if ch != "" && names[ch] != req[0] {
			cols = append(cols, "chassis")
		}
		for _, a := range strings.Fields(add) {
			if !slices.Contains(req[1:], names[a]) {
				cols = append(cols, "additional_chassis")
				break
			}
		}
		if len(cols) == 0 {
			continue
		}
		cleared := false
		for _, col := range cols {
			if _, err := run("ovn-sbctl", "clear", "port_binding", uuid, col); err != nil {
				errs = append(errs, fmt.Errorf("clear %s of %s: %w", col, port, err))
				continue
			}
			cleared = true
		}
		if cleared {
			log.Printf("cleared binding of port %s (%s): chassis %s, additional %s, requested %s", port, strings.Join(cols, ","), names[ch], add, strings.Join(req, ","))
			rebound = append(rebound, port)
		}
	}
	return rebound, errors.Join(errs...)
}

// guardMACBindings deletes lr-gw neighbor entries that do not match the address plan; any chassis may write MAC_Binding.
func guardMACBindings(run runner) error {
	rows, err := sbRows(run, "mac_binding", "_uuid,logical_port,ip,mac")
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range rows {
		uuid, port, ip, mac := f[0], f[1], f[2], strings.ToLower(f[3])
		var ok bool
		switch {
		case port == "lrp-transit":
			ok = ip == gw0IP && mac == gw0MAC
		case strings.HasPrefix(port, "lrp-mgmt-"):
			a, err := netip.ParseAddr(ip)
			ok = err == nil && a.Is4() && mac == netaddr.MAC(a)
		default:
			continue
		}
		if ok {
			continue
		}
		if _, err := run("ovn-sbctl", "destroy", "mac_binding", uuid); err != nil {
			errs = append(errs, fmt.Errorf("delete mac binding %s on %s: %w", ip, port, err))
			continue
		}
		log.Printf("deleted mac binding %s %s on %s", ip, mac, port)
	}
	return errors.Join(errs...)
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
