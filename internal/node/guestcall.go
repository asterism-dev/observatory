package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const GuestAgentPort = 1050

// GuestRequest is one guest agent call; it never carries keys or variables, the guest reads those from its seed.
type GuestRequest struct {
	Op      string   `json:"op"`
	Time    int64    `json:"time,omitempty"` // unix ns
	MACs    []string `json:"macs,omitempty"`
	Entropy []byte   `json:"entropy,omitempty"`
}

// CallGuest sends req to the guest agent through Firecracker's vsock uds and waits for its reply.
func CallGuest(ctx context.Context, uds string, req GuestRequest) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	c, err := dialUnix(ctx, uds)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	d, _ := ctx.Deadline()
	c.SetDeadline(d)
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", GuestAgentPort); err != nil {
		return err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("guest agent: %w", errors.Join(ctx.Err(), err))
	}
	if !strings.HasPrefix(line, "OK ") {
		return errors.New("guest agent: vsock connect refused")
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err = r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("guest agent: %w", errors.Join(ctx.Err(), err))
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("guest agent %s: %s", req.Op, reply.Error)
	}
	return nil
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
