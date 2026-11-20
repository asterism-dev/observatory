package fleet

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

// desiredRetry is how long a node waits for another desired list or underlay after building one failed.
var desiredRetry = 5 * time.Second

// integrationRetry is how long the hub waits before pushing again to an org whose integrations failed to resolve.
var integrationRetry = time.Minute

// imageRetryMin doubles up to imageRetryMax between pushes to a node with VMs whose image is not prepared yet, each with a fresh pull token.
var imageRetryMin, imageRetryMax = 30 * time.Second, 5 * time.Minute

// The chassis cert is re-signed from the node's start-up CSR when less than underlayRenewBefore is left, checked every underlayCheck.
var (
	underlayCheck       = 24 * time.Hour
	underlayRenewBefore = 30 * 24 * time.Hour
)

type conn struct {
	node, org uuid.UUID
	sess      *yamux.Session
	vms       map[string]bool            // IDs of the VMs in the last desired list; guarded by Fleet.mu
	accounts  map[string]map[string]bool // account IDs by integration ID in the last desired list; guarded by Fleet.mu
	// wake holds at most one pending push; the list is built when sent, so the latest state always wins.
	wake chan struct{}
	// update hands an update request to pushLoop; done closes when the connection's handler ends, pushLoop gone or not.
	update chan struct{}
	done   <-chan struct{}
	since  time.Time       // when attached; guarded by Fleet.mu
	seen   map[string]bool // IDs of VMs with a status on this connection; guarded by Fleet.mu
}

func (c *conn) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ServeConnect accepts a node: WebSocket → yamux, the first node-opened stream is the control stream and must start with register.
func (f *Fleet) ServeConnect(w http.ResponseWriter, r *http.Request) {
	cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	node, err := f.store.NodeByCredential(r.Context(), cred)
	if !ok || errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("node auth", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(websocket.NetConn(ctx, ws, websocket.MessageBinary), cfg)
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
		slog.Warn("node did not register", "node", node.ID)
		return
	}
	if err := f.register(ctx, node, *m.Register); err != nil {
		slog.Error("node register", "node", node.ID, "err", err)
		return
	}
	c := &conn{node: node.ID, org: node.OrgID, sess: sess, wake: make(chan struct{}, 1), update: make(chan struct{}), done: ctx.Done(), seen: map[string]bool{}}
	for _, st := range m.Register.VMs {
		c.seen[st.ID] = true
	}
	f.attach(c)
	defer f.detach(c)
	defer f.failSnapshots(node.ID)
	// A Disconnect racing with this handshake ran before attach; recheck so a revoked credential never stays connected.
	if _, err := f.store.NodeByCredential(ctx, cred); err != nil {
		return
	}
	slog.Info("node connected", "node", node.ID, "vms", len(m.Register.VMs))
	defer slog.Info("node disconnected", "node", node.ID)
	go f.pushLoop(ctx, c, proto.NewEncoder(ctrl), node, *m.Register)
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
		case m.Type == "metrics":
			f.NoteMetrics(ctx, node.ID, m.Metrics)
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
// node comes from the credential, never from reg, so a node can only get a chassis cert for itself.
func (f *Fleet) pushLoop(ctx context.Context, c *conn, enc *json.Encoder, node store.Node, reg proto.Register) {
	chassis := f.net != nil && reg.ChassisKey != "" && reg.ChassisCSR != ""
	var certExpiry, lastErrLog time.Time
	renew := time.NewTicker(underlayCheck)
	defer renew.Stop()
	// Only a failed write ends the loop; without an underlay the node still runs, and the next check retries.
	sendUnderlay := func() error {
		u, expiry, err := f.underlay(ctx, node, reg)
		if err != nil {
			// The short retry would otherwise log every few seconds.
			if time.Since(lastErrLog) >= 5*time.Minute {
				slog.Error("node underlay", "node", node.ID, "err", err)
				lastErrLog = time.Now()
			}
			if certExpiry.IsZero() {
				renew.Reset(desiredRetry)
			}
			return nil
		}
		certExpiry = expiry
		renew.Reset(underlayCheck)
		return enc.Encode(proto.Message{Type: "underlay", Underlay: u})
	}
	if chassis {
		if err := sendUnderlay(); err != nil {
			c.sess.Close()
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-renew.C:
			if chassis && time.Until(certExpiry) < underlayRenewBefore {
				if err := sendUnderlay(); err != nil {
					c.sess.Close()
					return
				}
			}
			continue
		case <-c.update:
			if err := enc.Encode(proto.Message{Type: "update"}); err != nil {
				c.sess.Close()
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
			c.sess.Close()
			return
		}
	}
}

func (f *Fleet) underlay(ctx context.Context, node store.Node, reg proto.Register) (*proto.Underlay, time.Time, error) {
	u, err := f.net.Underlay(ctx, node, reg.ChassisKey, reg.ChassisCSR)
	if err != nil {
		return nil, time.Time{}, err
	}
	b, _ := pem.Decode([]byte(u.Cert))
	if b == nil {
		return nil, time.Time{}, errors.New("chassis cert is not PEM")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, time.Time{}, err
	}
	return u, cert.NotAfter, nil
}
