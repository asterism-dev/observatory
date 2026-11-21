package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/asterism-dev/observatory/internal/proto"
)

type hubVM struct {
	Spec        proto.VMSpec
	NodeID      string
	Power       proto.Power
	StopMode    proto.StopMode
	Generation  uint64
	Provisioned bool
	Boot        *proto.Boot
	proto.VMStatus
}

type fakeHub struct {
	mu           sync.Mutex
	srv          *httptest.Server
	vmsByID      map[string]*hubVM
	regs         []proto.Register
	auth         []string
	conn         *json.Encoder
	ws           *websocket.Conn // guarded by connMu
	connMu       sync.Mutex
	seq          uint64
	nextID       int
	underlay     *proto.Underlay
	events       []string // "underlay" and "connect" in arrival order
	underlayReqs []proto.UnderlayRequest
	cpus         int // CPUs of VMs from create; zero means 1
	images       []proto.ImageStatus
	updates      []proto.UpdateReport
	replies      []proto.Message // type "processes"
	paths        []string        // connect request paths
	unhealthy    bool
	emergUpdate  bool
	emergencies  []proto.EmergencyReport
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{vmsByID: map[string]*hubVM{}, underlay: &proto.Underlay{Address: "10.198.0.11/16", HubKey: "hub-key", Endpoint: "gw.test:51820", SBRemote: "ssl:10.198.0.1:6642", CACert: "CA PEM", Cert: "CERT PEM"}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

// url is both the hub's public base URL and, in tests, the agent's controlBase.
func (h *fakeHub) url() string { return h.srv.URL }

func (h *fakeHub) serveHealth(w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	unhealthy := h.unhealthy
	h.mu.Unlock()
	switch {
	case req.Header.Get("Authorization") != "Bearer tok":
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case unhealthy:
		http.Error(w, "down", http.StatusServiceUnavailable)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *fakeHub) serveEmergency(w http.ResponseWriter, req *http.Request) {
	var body proto.EmergencyReport
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || req.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	h.emergencies = append(h.emergencies, body)
	reply := proto.EmergencyReply{Update: h.emergUpdate}
	h.mu.Unlock()
	_ = json.NewEncoder(w).Encode(reply)
}

func (h *fakeHub) serveUnderlay(w http.ResponseWriter, req *http.Request) {
	var body proto.UnderlayRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || req.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	h.events = append(h.events, "underlay")
	h.underlayReqs = append(h.underlayReqs, body)
	h.mu.Unlock()
	_ = json.NewEncoder(w).Encode(h.underlay)
}

func (h *fakeHub) serve(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/api/nodes/underlay":
		h.serveUnderlay(w, req)
		return
	case "/api/nodes/health":
		h.serveHealth(w, req)
		return
	case "/api/nodes/emergency":
		h.serveEmergency(w, req)
		return
	}
	h.mu.Lock()
	h.paths = append(h.paths, req.URL.Path)
	h.auth = append(h.auth, req.Header.Get("Authorization"))
	h.events = append(h.events, "connect")
	h.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(proto.MaxMessageBytes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl := websocket.NetConn(ctx, c, websocket.MessageBinary)
	dec := json.NewDecoder(ctrl)
	var m proto.Message
	if err := dec.Decode(&m); err != nil || m.Type != "register" || m.Register == nil {
		return
	}
	enc := proto.NewEncoder(ctrl)
	h.mu.Lock()
	for _, st := range m.Register.VMs {
		if vm, ok := h.vmsByID[st.ID]; ok && vm.NodeID != m.Register.NodeID {
			h.mu.Unlock()
			return
		}
	}
	h.regs = append(h.regs, *m.Register)
	for _, st := range m.Register.VMs {
		if vm, ok := h.vmsByID[st.ID]; ok {
			h.applyLocked(vm, st)
		}
	}
	h.mu.Unlock()
	h.connMu.Lock()
	h.conn, h.ws = enc, c
	h.connMu.Unlock()
	h.push()
	for {
		var m proto.Message
		if err := dec.Decode(&m); err != nil {
			return
		}
		if m.Type == "image" && m.Image != nil {
			h.mu.Lock()
			h.images = append(h.images, *m.Image)
			h.mu.Unlock()
		}
		if m.Type == "update" && m.Update != nil {
			h.mu.Lock()
			h.updates = append(h.updates, *m.Update)
			h.mu.Unlock()
		}
		if m.Type == "processes" {
			h.mu.Lock()
			h.replies = append(h.replies, m)
			h.mu.Unlock()
		}
		if m.Type == "status" && m.Status != nil {
			h.mu.Lock()
			if vm, ok := h.vmsByID[m.Status.ID]; ok {
				h.applyLocked(vm, *m.Status)
			}
			h.mu.Unlock()
		}
	}
}

func (h *fakeHub) applyLocked(vm *hubVM, st proto.VMStatus) {
	vm.VMStatus = st
	if !vm.Provisioned && slices.Contains([]proto.VMState{proto.StateStarting, proto.StateRunning, proto.StateStopped}, st.State) {
		vm.Provisioned = true
	}
}

func (h *fakeHub) send(m proto.Message) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.conn != nil {
		h.conn.Encode(m)
	}
}

func (h *fakeHub) push() {
	h.mu.Lock()
	h.seq++
	m := proto.Message{Type: "desired", Seq: h.seq}
	for _, vm := range h.vmsByID {
		m.Desired = append(m.Desired, proto.DesiredVM{Spec: vm.Spec, Power: vm.Power, StopMode: vm.StopMode, Generation: vm.Generation, Provisioned: vm.Provisioned, Boot: vm.Boot})
	}
	slices.SortFunc(m.Desired, func(a, b proto.DesiredVM) int { return strings.Compare(a.Spec.ID, b.Spec.ID) })
	h.mu.Unlock()
	h.send(m)
}

func (h *fakeHub) create(name string) hubVM { return h.createWith(name, nil) }

func (h *fakeHub) createWith(name string, edit func(*hubVM)) hubVM {
	h.mu.Lock()
	h.nextID++
	id := "vm" + string(rune('a'+h.nextID))
	cpus := max(h.cpus, 1)
	vm := &hubVM{
		Spec:   proto.VMSpec{ID: id, Name: name, CPUs: cpus, MemoryMB: 1024, DiskGB: 10},
		Boot:   &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAA"}},
		NodeID: "n1", Power: proto.PowerRunning, Generation: 1, VMStatus: proto.VMStatus{ID: id, State: proto.StatePending},
	}
	if edit != nil {
		edit(vm)
	}
	h.vmsByID[id] = vm
	out := *vm
	h.mu.Unlock()
	h.push()
	return out
}

func (h *fakeHub) delete(id string) {
	h.mu.Lock()
	delete(h.vmsByID, id)
	h.mu.Unlock()
	h.push()
}

func (h *fakeHub) setPower(id string, power proto.Power, mode proto.StopMode) {
	h.mu.Lock()
	vm := h.vmsByID[id]
	vm.Power, vm.StopMode = power, mode
	if power == proto.PowerRunning && vm.Error == proto.ErrVMDirMissing {
		vm.Provisioned = false
	}
	vm.Generation++
	h.mu.Unlock()
	h.push()
}

// setBoot changes keys or variables; like the real hub it keeps the generation.
func (h *fakeHub) setBoot(id string, boot *proto.Boot) {
	h.mu.Lock()
	h.vmsByID[id].Boot = boot
	h.mu.Unlock()
	h.push()
}

func (h *fakeHub) vm(id string) (hubVM, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if vm, ok := h.vmsByID[id]; ok {
		return *vm, true
	}
	return hubVM{}, false
}

func (h *fakeHub) vms() []hubVM {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hubVM
	for _, vm := range h.vmsByID {
		out = append(out, *vm)
	}
	return out
}

func (h *fakeHub) processReplies() []proto.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.replies)
}

func (h *fakeHub) registers() []proto.Register {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.regs)
}

func (h *fakeHub) authHeaders() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.auth)
}

func (h *fakeHub) gotImages() []proto.ImageStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.images)
}

// drop ends the node's connection; the agent reconnects after reconnectDelay.
func (h *fakeHub) drop() {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	h.ws.CloseNow()
}

func (h *fakeHub) sendUpdate() { h.send(proto.Message{Type: "update"}) }

func (h *fakeHub) gotUpdates() []proto.UpdateReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.updates)
}

func (h *fakeHub) connectPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.paths)
}

func (h *fakeHub) setHealthy(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unhealthy = !ok
}

func (h *fakeHub) setEmergencyUpdate(update bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emergUpdate = update
}

func (h *fakeHub) gotEmergencies() []proto.EmergencyReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.emergencies)
}
