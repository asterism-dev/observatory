package node

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	underlayCheck       = 24 * time.Hour
	underlayRenewBefore = 30 * 24 * time.Hour
)

var underlayClient = &http.Client{Timeout: 30 * time.Second}

// refreshUnderlay trades the chassis key and a fresh CSR for the underlay config and applies it.
func (a *Agent) refreshUnderlay(ctx context.Context) error {
	body, err := json.Marshal(proto.UnderlayRequest{ChassisKey: a.ChassisKey, ChassisCSR: a.ChassisCSR})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.HubURL+"/api/nodes/underlay", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Credential)
	req.Header.Set("Content-Type", "application/json")
	resp, err := underlayClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("underlay request: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var u proto.Underlay
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return fmt.Errorf("underlay response: %w", err)
	}
	a.mu.Lock()
	a.underlayCert = u.Cert
	a.mu.Unlock()
	if a.mesh != nil {
		a.mesh.setGateway(u.HubKey)
	}
	go a.startChassis(ctx, u)
	return nil
}

func (a *Agent) renewUnderlay(ctx context.Context) {
	t := time.NewTicker(underlayCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		cert := a.underlayCert
		a.mu.Unlock()
		if !certExpiresSoon(cert, time.Now()) {
			continue
		}
		if err := a.refreshUnderlay(ctx); err != nil {
			slog.Warn("underlay renewal failed", "err", err)
		}
	}
}

func certExpiresSoon(certPEM string, now time.Time) bool {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		return true
	}
	c, err := x509.ParseCertificate(b.Bytes)
	return err != nil || c.NotAfter.Sub(now) < underlayRenewBefore
}
