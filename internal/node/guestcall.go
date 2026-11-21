package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const GuestAgentPort = 1050

// maxGuestReply bounds what a guest, which root inside the VM controls, can make the node read.
const maxGuestReply = 1 << 20

// GuestRequest is one guest agent call; it never carries keys or variables, the guest reads those from its seed.
type GuestRequest struct {
	Op      string   `json:"op"`
	Time    int64    `json:"time,omitempty"` // unix ns
	MACs    []string `json:"macs,omitempty"`
	Entropy []byte   `json:"entropy,omitempty"`
	// Certs are the public egress CA certs by id the guest trusts; never keys.
	Certs map[string]string `json:"certs,omitempty"`
}

// GuestReply is the agent's answer; Version is set on ping, Processes on processes.
type GuestReply struct {
	OK        bool                 `json:"ok"`
	Error     string               `json:"error"`
	Version   string               `json:"version"`
	Processes []proto.GuestProcess `json:"processes"`
}

// CallGuest sends req to the guest agent through Firecracker's vsock uds and waits for its reply.
func CallGuest(ctx context.Context, uds string, req GuestRequest) (GuestReply, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	c, err := dialUnix(ctx, uds)
	if err != nil {
		return GuestReply{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	d, _ := ctx.Deadline()
	c.SetDeadline(d)
	b, err := json.Marshal(req)
	if err != nil {
		return GuestReply{}, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", GuestAgentPort); err != nil {
		return GuestReply{}, err
	}
	r := bufio.NewReader(io.LimitReader(c, maxGuestReply))
	line, err := r.ReadString('\n')
	if err != nil {
		return GuestReply{}, fmt.Errorf("guest agent: %w", errors.Join(ctx.Err(), err))
	}
	if !strings.HasPrefix(line, "OK ") {
		return GuestReply{}, errors.New("guest agent: vsock connect refused")
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return GuestReply{}, err
	}
	line, err = r.ReadString('\n')
	if err != nil {
		return GuestReply{}, fmt.Errorf("guest agent: %w", errors.Join(ctx.Err(), err))
	}
	var reply GuestReply
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		return GuestReply{}, fmt.Errorf("guest agent: %w", err)
	}
	if !reply.OK {
		return reply, fmt.Errorf("guest agent %s: %s", req.Op, reply.Error)
	}
	return reply, nil
}

// dialUnix reaches a long jail path through its directory's fd, held open only for the dial, as unix socket paths are limited to 107 bytes.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	if len(path) < 100 {
		return d.DialContext(ctx, "unix", path)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path)))
}
