package guestinit

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/version"
)

const (
	agentPort         = 1050
	maxAgentRequest   = 64 << 10
	maxAgentConns     = 4
	maxAgentMACs      = 16
	maxAgentEntropy   = 1024
	minEntropy        = 16
	pingBootTimeout   = 2 * time.Minute
	requestReadWindow = 10 * time.Second
)

var macRE = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// Agent serves the node's requests on vsock; every system-touching step is a field so tests can fake it.
type Agent struct {
	Root          Root
	Seed          func() (proto.GuestSeed, error)
	SetMACs       func(macs []string) error
	SetClock      func(ns int64) error
	ReseedRNG     func(entropy []byte) error
	SetHostname   func(name string) error
	ConfigureNICs func([]proto.GuestNIC) error
	GrowRoot      func() error
	MountDisks    func(Root, []proto.GuestDisk) error
	// Sync and StopDesktop are nil-safe: the integrations and desktop tasks are wired in by Run.
	Sync        func()
	StopDesktop func()
	Proc        string

	mu        sync.Mutex // serializes takeoff, resume and trust
	stateMu   sync.RWMutex
	seedKeys  []string
	ssh       *SSHServer
	env       map[string]string
	readyOnce sync.Once
	ready     chan struct{}
	initOnce  sync.Once
}

type agentRequest struct {
	Op      string          `json:"op"`
	Time    json.RawMessage `json:"time"`
	MACs    []string        `json:"macs"`
	Entropy string          `json:"entropy"`
	Certs   json.RawMessage `json:"certs"`
}

type agentReply struct {
	OK        bool                 `json:"ok"`
	Error     string               `json:"error,omitempty"`
	Version   string               `json:"version,omitempty"`
	Processes []proto.GuestProcess `json:"processes,omitempty"`
}

func (a *Agent) readyChan() chan struct{} {
	a.initOnce.Do(func() { a.ready = make(chan struct{}) })
	return a.ready
}

func (a *Agent) isReady() bool {
	select {
	case <-a.readyChan():
		return true
	default:
		return false
	}
}

// SetReady lets ping succeed; Run calls it once the guest is set up.
func (a *Agent) SetReady() { a.readyOnce.Do(func() { close(a.readyChan()) }) }

// SeedKeys is the current seed's SSH keys, the SSH server's source.
func (a *Agent) SeedKeys() []string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.seedKeys
}

// Env is the environment SSH sessions get.
func (a *Agent) Env() map[string]string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.env
}

// SetSSH registers the server that takeoff hands new host keys to.
func (a *Agent) SetSSH(s *SSHServer) {
	a.stateMu.Lock()
	a.ssh = s
	a.stateMu.Unlock()
}

func (a *Agent) setSeedKeys(keys []string) {
	a.stateMu.Lock()
	a.seedKeys = keys
	a.stateMu.Unlock()
}

// applyEnv writes the guest environment and makes it the one SSH sessions get; a failed write is logged and the merged env still used.
func (a *Agent) applyEnv(seed proto.GuestSeed) map[string]string {
	if err := a.Root.WriteContext(seed.Context); err != nil {
		fmt.Fprintln(os.Stderr, "agent: context:", err)
	}
	env, err := a.Root.WriteEnv(seed.Env, seed.Image.Env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent: environment:", err)
	}
	if env == nil {
		env = map[string]string{"PATH": platformBin + ":" + defaultPath}
	}
	a.stateMu.Lock()
	a.env = env
	a.stateMu.Unlock()
	return env
}

func (a *Agent) installCAs(seed proto.GuestSeed) {
	if _, err := a.Root.InstallCAs(seed.EgressCAs); err != nil {
		fmt.Fprintln(os.Stderr, "agent: egress CAs:", err)
	}
}

// Handle runs one request line and returns the JSON reply, without newline.
func (a *Agent) Handle(line []byte) []byte {
	reply := a.handle(line)
	b, _ := json.Marshal(reply)
	return b
}

func (a *Agent) handle(line []byte) agentReply {
	req, err := parseAgentRequest(line)
	if err != nil {
		return agentReply{Error: err.Error()}
	}
	// A poll during boot must not hold one of the few agent slots for minutes.
	if req.Op == "processes" && !a.isReady() {
		return agentReply{Error: "booting"}
	}
	// Every op waits for Run's setup, or a trust could be overwritten by the setup's older seed.
	select {
	case <-a.readyChan():
	case <-time.After(pingBootTimeout):
		return agentReply{Error: "booting"}
	}
	switch req.Op {
	case "ping":
		return agentReply{OK: true, Version: version.Version}
	case "processes":
		// Read-only, so it never waits behind a takeoff.
		ps, err := listProcesses(a.Proc, a.Root.join("etc/passwd"), time.Sleep)
		if err != nil {
			return agentReply{Error: err.Error()}
		}
		return agentReply{OK: true, Processes: ps}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch req.Op {
	case "takeoff":
		err = a.takeoff(req)
	case "resume":
		err = a.resume(req)
	case "trust":
		err = a.trust(req)
	default:
		return agentReply{Error: "unknown op"}
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return agentReply{Error: msg}
	}
	return agentReply{OK: true}
}

type parsedRequest struct {
	Op      string
	Time    int64
	MACs    []string
	Entropy []byte
	Certs   map[string]string
}

func parseAgentRequest(line []byte) (parsedRequest, error) {
	var raw agentRequest
	if err := json.Unmarshal(line, &raw); err != nil || raw.Op == "" {
		return parsedRequest{}, errors.New("bad request")
	}
	req := parsedRequest{Op: raw.Op, MACs: raw.MACs}
	if len(raw.Time) > 0 {
		t, err := strconv.ParseInt(string(raw.Time), 10, 64)
		if err != nil || t < 0 {
			return req, errors.New("bad time")
		}
		req.Time = t
	}
	if len(req.MACs) > maxAgentMACs {
		return req, errors.New("bad macs")
	}
	for _, m := range req.MACs {
		if !macRE.MatchString(m) {
			return req, errors.New("bad macs")
		}
	}
	if len(raw.Entropy) > maxAgentEntropy {
		return req, errors.New("bad entropy")
	}
	var err error
	if req.Entropy, err = base64.StdEncoding.DecodeString(raw.Entropy); err != nil {
		return req, errors.New("bad entropy")
	}
	if raw.Op == "trust" && len(raw.Certs) > 0 {
		if string(raw.Certs) == "null" || json.Unmarshal(raw.Certs, &req.Certs) != nil {
			return req, errors.New("bad certs")
		}
	}
	return req, nil
}

func (a *Agent) resume(req parsedRequest) error {
	if err := a.setClock(req.Time); err != nil {
		return err
	}
	seed, err := a.Seed()
	if err != nil {
		return err
	}
	a.setSeedKeys(seed.SSHKeys)
	a.installCAs(seed)
	a.applyEnv(seed)
	return nil
}

func (a *Agent) trust(req parsedRequest) error {
	_, err := a.Root.InstallCAs(req.Certs)
	return err
}

func (a *Agent) setClock(ns int64) error {
	if ns == 0 {
		return nil
	}
	return a.SetClock(ns)
}

// takeoff gives a restored template its own identity: MACs first, as anything sent once the clock moves must not carry the template's.
func (a *Agent) takeoff(req parsedRequest) error {
	if err := a.SetMACs(req.MACs); err != nil {
		return err
	}
	if err := a.setClock(req.Time); err != nil {
		return err
	}
	if len(req.Entropy) < minEntropy {
		return errors.New("takeoff needs host entropy")
	}
	if err := a.ReseedRNG(req.Entropy); err != nil {
		return err
	}
	seed, err := a.Seed()
	if err != nil {
		return err
	}
	// The new machine-id and host keys come before the network, so the template's identity never answers on the VM's address.
	if err := a.Root.WriteHostname(seed.Hostname, seed.InstanceID); err != nil {
		return err
	}
	if err := a.SetHostname(seed.Hostname); err != nil {
		return err
	}
	if err := a.ConfigureNICs(seed.NICs); err != nil {
		return err
	}
	if err := a.Root.WriteResolv(seed.NICs); err != nil {
		return err
	}
	keys, err := a.Root.HostKeys(true)
	if err != nil {
		return err
	}
	a.stateMu.RLock()
	srv := a.ssh
	a.stateMu.RUnlock()
	if srv != nil {
		srv.SetHostKeys(keys)
	}
	a.setSeedKeys(seed.SSHKeys)
	if err := a.GrowRoot(); err != nil {
		return err
	}
	if err := a.MountDisks(a.Root, seed.Disks); err != nil {
		return err
	}
	a.installCAs(seed)
	a.applyEnv(seed)
	if a.Sync != nil {
		a.Sync()
	}
	// A fork's browser would otherwise keep the source's profile lock and hostname.
	if a.StopDesktop != nil {
		a.StopDesktop()
	}
	return nil
}

// agentConn is what the agent needs of a connection; net.Conn and a nonblocking os.File both fit.
type agentConn interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// ServeAgent accepts until accept fails; accept reports whether the peer is the host, as guest users could reach the agent over vsock loopback.
func (a *Agent) ServeAgent(accept func() (c agentConn, fromHost bool, err error)) error {
	slots := make(chan struct{}, maxAgentConns)
	for {
		c, fromHost, err := accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		if !fromHost {
			<-slots
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			a.serveAgentConn(c)
		}()
	}
}

func (a *Agent) serveAgentConn(c agentConn) {
	defer c.Close()
	line, err := readRequestLine(c)
	var reply []byte
	if err != nil {
		reply, _ = json.Marshal(agentReply{Error: err.Error()})
	} else {
		reply = a.Handle(line)
	}
	_ = c.SetWriteDeadline(time.Now().Add(requestReadWindow))
	_, _ = c.Write(append(reply, '\n'))
}

func readRequestLine(c agentConn) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(requestReadWindow))
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for i, b := range buf {
			if b == '\n' {
				_ = c.SetReadDeadline(time.Time{})
				return buf[:i], nil
			}
		}
		if len(buf) > maxAgentRequest {
			return nil, errors.New("request too large")
		}
		if err != nil {
			return nil, errors.New("incomplete request")
		}
	}
}
