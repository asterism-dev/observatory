package node

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"regexp"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

// TokenPort is the guest-initiated vsock port of the token service.
const TokenPort = 1051

const (
	TokenNotConnected = "not-connected"
	TokenUnavailable  = "unavailable"
)

const (
	maxTokenRequest  = 4 << 10
	tokenTimeout     = 2 * time.Second
	maxTokenRequests = 16
)

var tokenKind = regexp.MustCompile(`^[a-z]{1,16}$`)

// TokenLookup returns vmID's token of kind, or TokenNotConnected/TokenUnavailable with an empty token.
type TokenLookup func(vmID, kind string) (proto.IntegrationToken, string)

// TokenServer is a provider that serves each VM's token socket with the lookup.
type TokenServer interface{ SetTokenLookup(TokenLookup) }

// ServeTokens answers token requests on l, the listener of vmID only, until l is closed.
func ServeTokens(l net.Listener, vmID string, lookup TokenLookup) {
	slots := make(chan struct{}, maxTokenRequests)
	var delay time.Duration
	for {
		c, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		// EMFILE and the like pass; returning would leave the VM without a token service until it restarts.
		if err != nil {
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			slog.Warn("token accept failed", "vm", vmID, "err", err)
			time.Sleep(delay)
			continue
		}
		delay = 0
		select {
		case slots <- struct{}{}:
			go func() {
				// The slot is free before the client sees the close, so a client that saw it can count on the slot.
				defer c.Close()
				defer func() { <-slots }()
				serveToken(c, vmID, lookup)
			}()
		default:
			c.Close()
		}
	}
}

func serveToken(c net.Conn, vmID string, lookup TokenLookup) {
	c.SetDeadline(time.Now().Add(tokenTimeout))
	line, err := bufio.NewReader(io.LimitReader(c, maxTokenRequest+1)).ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return
	}
	var req struct{ Op, Kind string }
	kind, result := "?", TokenNotConnected
	var tok proto.IntegrationToken
	if len(line) <= maxTokenRequest && json.Unmarshal(line, &req) == nil && req.Op == "token" && tokenKind.MatchString(req.Kind) {
		kind = req.Kind
		tok, result = lookup(vmID, kind)
	}
	var reply any
	if result == "" {
		reply = struct {
			OK        bool       `json:"ok"`
			Token     string     `json:"token"`
			ExpiresAt *time.Time `json:"expiresAt"`
			Project   string     `json:"project,omitempty"`
		}{true, tok.Token, tok.ExpiresAt, tok.Project}
		slog.Info("token served", "vm", vmID, "kind", kind, "result", "ok")
	} else {
		reply = struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}{false, result}
		slog.Info("token served", "vm", vmID, "kind", kind, "result", result)
	}
	b, _ := json.Marshal(reply)
	c.Write(append(b, '\n'))
	// Closing with unread input makes Linux reset the connection and drop the reply; half-close, then drain until the client hangs up.
	if cw, ok := c.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
		io.Copy(io.Discard, io.LimitReader(c, maxTokenRequest))
	}
}

// integrationToken looks up a VM's token from the hub's last push; the node keeps none across restarts.
func (a *Agent) integrationToken(vmID, kind string) (proto.IntegrationToken, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.vms[vmID]
	if !ok || e.file.Boot == nil || e.file.IntegrationsUnresolved {
		return proto.IntegrationToken{}, TokenUnavailable
	}
	for _, t := range e.file.Integrations {
		if t.Kind == kind {
			return t, ""
		}
	}
	return proto.IntegrationToken{}, TokenNotConnected
}
