package fleet

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

// desiredRetry is how long a node waits for another desired list after building one failed.
var desiredRetry = 5 * time.Second

// integrationRetry is how long the hub waits before pushing again to an org whose integrations failed to resolve.
var integrationRetry = time.Minute

// imageRetryMin doubles up to imageRetryMax between pushes to a node with VMs whose image is not prepared yet, each with a fresh pull token.
var imageRetryMin, imageRetryMax = 30 * time.Second, 5 * time.Minute

type conn struct {
	node, org uuid.UUID
	ws        *websocket.Conn
	vms       map[string]bool            // IDs of the VMs in the last desired list; guarded by Fleet.mu
	accounts  map[string]map[string]bool // account IDs by integration ID in the last desired list; guarded by Fleet.mu
	// wake holds at most one pending push; the list is built when sent, so the latest state always wins.
	wake chan struct{}
	// update hands an update request to pushLoop; done closes when the connection's handler ends, pushLoop gone or not.
	update chan struct{}
	done   <-chan struct{}
	since  time.Time       // when attached; guarded by Fleet.mu
	seen   map[string]bool // IDs of VMs with a status on this connection; guarded by Fleet.mu
	// requests hands hub→node requests to pushLoop; their replies go to pending by ReqID, guarded by Fleet.mu.
	requests chan proto.Message
	pending  map[string]chan proto.Message
}

func (c *conn) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// underlayNode returns the Bearer credential's node, but only for a request the gateway's control proxy forwarded from that node's underlay address; otherwise a status to refuse with.
func (f *Fleet) underlayNode(r *http.Request) (store.Node, int) {
	if f.net == nil {
		return store.Node{}, http.StatusServiceUnavailable
	}
	if !f.net.TokenOK(r.Header.Get(proto.HeaderGateway)) {
		return store.Node{}, http.StatusUnauthorized
	}
	cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	node, err := f.store.NodeByCredential(r.Context(), cred)
	if !ok || errors.Is(err, store.ErrNotFound) {
		return store.Node{}, http.StatusUnauthorized
	}
	if err != nil {
		slog.Error("node auth", "err", err)
		return store.Node{}, http.StatusInternalServerError
	}
	// The proxy stamps an empty peer when it cannot parse the remote address; that never matches.
	peer, err := netip.ParseAddr(r.Header.Get(proto.HeaderUnderlayPeer))
	if err != nil || !node.UnderlayIP.IsValid() || peer.Unmap() != node.UnderlayIP {
		slog.Warn("node control from wrong underlay peer", "node", node.ID)
		return store.Node{}, http.StatusForbidden
	}
	return node, http.StatusOK
}

// ServeHealth answers a node's health check over the underlay and records when it last succeeded.
func (f *Fleet) ServeHealth(w http.ResponseWriter, r *http.Request) {
	node, status := f.underlayNode(r)
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	f.mu.Lock()
	f.healthSeen[node.ID] = time.Now()
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

const maxVersionLen = 64

// ServeEmergency takes a node's emergency poll over the public HTTPS URL while its underlay is down; the reply hands over an update an update run waits to deliver.
func (f *Fleet) ServeEmergency(w http.ResponseWriter, r *http.Request) {
	cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	node, err := f.store.NodeByCredential(r.Context(), cred)
	if !ok || errors.Is(err, store.ErrNotFound) {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("node auth", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	var rep proto.EmergencyReport
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil || len(rep.Version) > maxVersionLen {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// Without a connection no register stores the version, so a node updated here would otherwise stay outdated.
	if rep.Version != "" && rep.Version != node.Version {
		if err := f.store.SetNodeVersion(r.Context(), node.ID, rep.Version); err != nil {
			slog.Error("store emergency version", "node", node.ID, "err", err)
		}
	}
	f.mu.Lock()
	if !f.underlayDown(node.ID) {
		slog.Warn("node underlay down", "node", node.ID, "reason", rep.Reason)
	}
	f.emergency[node.ID] = emergencySeen{at: clock(), version: rep.Version}
	if rep.UpdateError != "" {
		slog.Warn("node update failed", "node", node.ID, "err", rep.UpdateError)
		f.updateErrs[node.ID] = rep.UpdateError
	}
	reply := proto.EmergencyReply{Update: f.emergencyUpd[node.ID]}
	delete(f.emergencyUpd, node.ID)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// ServeConnect accepts a node through the gateway's control proxy: one WebSocket carrying JSON messages, starting with register.
func (f *Fleet) ServeConnect(w http.ResponseWriter, r *http.Request) {
	node, status := f.underlayNode(r)
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	// The hijacked connection would otherwise keep the server's per-request deadlines.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(proto.MaxMessageBytes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	dec := json.NewDecoder(ctrl)
	var m proto.Message
	if err := dec.Decode(&m); err != nil || m.Type != "register" || m.Register == nil {
		slog.Warn("node did not register", "node", node.ID)
		return
	}
	if err := f.register(ctx, node, *m.Register); err != nil {
		slog.Error("node register", "node", node.ID, "err", err)
		return
	}
	c := &conn{node: node.ID, org: node.OrgID, ws: ws, wake: make(chan struct{}, 1), update: make(chan struct{}), done: ctx.Done(), seen: map[string]bool{},
		requests: make(chan proto.Message), pending: map[string]chan proto.Message{}}
	for _, st := range m.Register.VMs {
		c.seen[st.ID] = true
	}
	f.attach(c)
	defer f.detach(c)
	defer f.failSnapshots(node.ID)
	// A Disconnect racing with this handshake ran before attach; recheck so a revoked credential never stays connected.
	cred, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, err := f.store.NodeByCredential(ctx, cred); err != nil {
		return
	}
	slog.Info("node connected", "node", node.ID, "vms", len(m.Register.VMs))
	defer slog.Info("node disconnected", "node", node.ID)
	go f.pushLoop(ctx, c, proto.NewEncoder(ctrl), *m.Register)
	c.kick()
	for {
		var m proto.Message
		if err := dec.Decode(&m); err != nil {
			return
		}
		switch {
		case m.Type == "status" && m.Status != nil:
			if err := f.store.SetVMStatus(ctx, node.ID, *m.Status); err != nil {
				slog.Error("vm status", "node", node.ID, "vm", m.Status.ID, "err", err)
			} else {
				f.mu.Lock()
				c.seen[m.Status.ID] = true // after SetVMStatus, so an update run never pairs it with the stored state from before
				f.mu.Unlock()
			}
			f.noteDiskErrors(c, *m.Status)
			if changed, err := f.store.ApplySnapshotStatus(ctx, node.ID, *m.Status); err != nil {
				slog.Error("vm snapshot status", "node", node.ID, "vm", m.Status.ID, "err", err)
			} else if changed {
				f.NotifyOrg(node.OrgID)
			}
		case m.Type == "update" && m.Update != nil:
			slog.Warn("node update failed", "node", node.ID, "err", m.Update.Error)
			f.mu.Lock()
			f.updateErrs[node.ID] = cmp.Or(m.Update.Error, "update failed")
			f.mu.Unlock()
		case m.Type == "processes":
			f.mu.Lock()
			reply := c.pending[m.ReqID]
			f.mu.Unlock()
			if reply != nil {
				select {
				case reply <- m:
				default:
				}
			}
		case m.Type == "metrics":
			// Old nodes still send these; VM usage now comes from the telemetry store.
		case m.Type == "usage":
			f.NoteUsage(ctx, node, m.Usage)
		case m.Type == "disks":
			f.confirmReleases(ctx, node, m.DisksInUse, m.AppliedSeq)
		case m.Type == "image" && m.Image != nil:
			if err := f.store.SetNodeImage(ctx, node.ID, *m.Image); err != nil {
				slog.Error("node image", "node", node.ID, "err", err)
			} else {
				f.NotifyOrg(node.OrgID)
			}
		}
	}
}

// confirmReleases drops the node's releasing exports that inUse and appliedSeq, both from one node message, show it has let go of.
func (f *Fleet) confirmReleases(ctx context.Context, node store.Node, inUse []string, appliedSeq uint64) {
	ids := make([]uuid.UUID, 0, len(inUse))
	for _, s := range inUse {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	released, err := f.store.ConfirmReleases(ctx, node.ID, ids, appliedSeq)
	if err != nil {
		slog.Error("confirm disk releases", "node", node.ID, "err", err)
		return
	}
	if len(released) > 0 {
		slog.Info("disk release confirmed", "node", node.ID, "disks", released)
		f.NotifyOrg(node.OrgID)
	}
}

func (f *Fleet) register(ctx context.Context, n store.Node, reg proto.Register) error {
	node := n.ID
	var machine json.RawMessage
	if reg.Machine != nil {
		var err error
		if machine, err = json.Marshal(reg.Machine); err != nil {
			return err
		}
	}
	if err := f.store.UpdateNodeInfo(ctx, node, reg.CPUs, reg.MemoryMB, machine, reg.Version, reg.Hypervisor); err != nil {
		return err
	}
	if reg.Image != nil {
		if err := f.store.SetNodeImage(ctx, node, *reg.Image); err != nil {
			return err
		}
	}
	// Statuses land before the first desired list so it carries the right provisioned flags.
	for _, st := range reg.VMs {
		if err := f.store.SetVMStatus(ctx, node, st); err != nil {
			return err
		}
		if _, err := f.store.ApplySnapshotStatus(ctx, node, st); err != nil {
			return err
		}
	}
	f.confirmReleases(ctx, n, reg.DisksInUse, reg.AppliedSeq)
	return nil
}

// pushLoop is the connection's only writer, so a slow node blocks nothing but itself.
func (f *Fleet) pushLoop(ctx context.Context, c *conn, enc *json.Encoder, reg proto.Register) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.update:
			if err := enc.Encode(proto.Message{Type: "update"}); err != nil {
				c.ws.CloseNow()
				return
			}
			continue
		case m := <-c.requests:
			if err := enc.Encode(m); err != nil {
				c.ws.CloseNow()
				return
			}
			continue
		case <-c.wake:
		}
		desired, releasing, err := f.desired(ctx, c.org, c.node)
		if err != nil {
			slog.Error("build desired list", "node", c.node, "err", err)
			time.AfterFunc(desiredRetry, c.kick)
			continue
		}
		vms := make(map[string]bool, len(desired))
		accounts := map[string]map[string]bool{}
		for _, d := range desired {
			vms[d.Spec.ID] = true
			if d.Egress != nil {
				desiredAccounts(accounts, d.Egress.Secrets)
			}
		}
		f.mu.Lock()
		f.dropDiskErrors(c, vms)
		c.vms, c.accounts = vms, accounts
		f.mu.Unlock()
		// Register statuses arrive before the node's VMs are known; any later status is newer.
		for _, st := range reg.VMs {
			f.noteDiskErrors(c, st)
		}
		reg.VMs = nil
		seq := f.seq.Add(1)
		// Marked before sending, or the node's answer could beat the mark; an unsent seq is harmless, every later list lacks the disk too.
		if len(releasing) > 0 {
			if err := f.store.MarkReleaseSeq(ctx, c.node, releasing, seq); err != nil {
				slog.Error("mark disk release", "node", c.node, "err", err)
				time.AfterFunc(desiredRetry, c.kick)
			}
		}
		if err := enc.Encode(proto.Message{Type: "desired", Desired: desired, Seq: seq}); err != nil {
			c.ws.CloseNow()
			return
		}
	}
}
