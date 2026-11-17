package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

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
	mu       sync.Mutex
	srv      *httptest.Server
	vmsByID  map[string]*hubVM
	regs     []proto.Register
	auth     []string
	conn     *json.Encoder
	connMu   sync.Mutex
	seq      uint64
	nextID   int
	underlay *proto.Underlay
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{vmsByID: map[string]*hubVM{}, underlay: &proto.Underlay{Address: "10.198.0.11/24", HubKey: "hub-key", SBRemote: "tcp:10.198.0.1:6642", Port: 51820}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) url() string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/nodes/connect"
}

func (h *fakeHub) serve(w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	h.auth = append(h.auth, req.Header.Get("Authorization"))
	h.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(websocket.NetConn(ctx, c, websocket.MessageBinary), cfg)
	if err != nil {
		return
	}
	defer sess.Close()
	ctrl, err := sess.Accept()
	if err != nil {
		return
	}
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
	h.conn = enc
	h.connMu.Unlock()
	h.push()
	if m.Register.ChassisKey != "" {
		h.send(proto.Message{Type: "underlay", Underlay: h.underlay})
	}
	for {
		var m proto.Message
		if err := dec.Decode(&m); err != nil {
			return
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

func (h *fakeHub) create(name string) hubVM {
	h.mu.Lock()
	h.nextID++
	id := "vm" + string(rune('a'+h.nextID))
	vm := &hubVM{
		Spec:   proto.VMSpec{ID: id, Name: name, CPUs: 1, MemoryMB: 1024, DiskGB: 10},
		Boot:   &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAA"}},
		NodeID: "n1", Power: proto.PowerRunning, Generation: 1, VMStatus: proto.VMStatus{ID: id, State: proto.StatePending},
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
