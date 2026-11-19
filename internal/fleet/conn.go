package fleet

import (
	"context"
	"encoding/json"
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

// desiredRetry is how long a node waits for another desired list after building one failed.
var desiredRetry = 5 * time.Second

type conn struct {
	node, org uuid.UUID
	sess      *yamux.Session
	// wake holds at most one pending push; the list is built when sent, so the latest state always wins.
	wake chan struct{}
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
	if err := f.register(ctx, node.ID, *m.Register); err != nil {
		slog.Error("node register", "node", node.ID, "err", err)
		return
	}
	c := &conn{node: node.ID, org: node.OrgID, sess: sess, wake: make(chan struct{}, 1)}
	f.attach(c)
	defer f.detach(c)
	// A Disconnect racing with this handshake ran before attach; recheck so a revoked credential never stays connected.
	if _, err := f.store.NodeByCredential(ctx, cred); err != nil {
		return
	}
	slog.Info("node connected", "node", node.ID, "vms", len(m.Register.VMs))
	defer slog.Info("node disconnected", "node", node.ID)
	go f.pushLoop(ctx, c, proto.NewEncoder(ctrl))
	c.kick()
	for {
		var m proto.Message
		if err := dec.Decode(&m); err != nil {
			return
		}
		if m.Type == "status" && m.Status != nil {
			if err := f.store.SetVMStatus(ctx, node.ID, *m.Status); err != nil {
				slog.Error("vm status", "node", node.ID, "vm", m.Status.ID, "err", err)
			}
		}
	}
}

func (f *Fleet) register(ctx context.Context, node uuid.UUID, reg proto.Register) error {
	var machine json.RawMessage
	if reg.Machine != nil {
		var err error
		if machine, err = json.Marshal(reg.Machine); err != nil {
			return err
		}
	}
	if err := f.store.UpdateNodeInfo(ctx, node, reg.CPUs, reg.MemoryMB, machine, reg.Version); err != nil {
		return err
	}
	// Statuses land before the first desired list so it carries the right provisioned flags.
	for _, st := range reg.VMs {
		if err := f.store.SetVMStatus(ctx, node, st); err != nil {
			return err
		}
	}
	return nil
}

// pushLoop is the connection's only writer, so a slow node blocks nothing but itself.
func (f *Fleet) pushLoop(ctx context.Context, c *conn, enc *json.Encoder) {
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		}
		desired, err := f.desired(ctx, c.org, c.node)
		if err != nil {
			slog.Error("build desired list", "node", c.node, "err", err)
			time.AfterFunc(desiredRetry, c.kick)
			continue
		}
		seq++
		if err := enc.Encode(proto.Message{Type: "desired", Desired: desired, Seq: seq}); err != nil {
			c.sess.Close()
			return
		}
	}
}
