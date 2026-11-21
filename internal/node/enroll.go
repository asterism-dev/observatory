package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Enroll redeems a one-time enrollment token at the hub for the node id and credential.
func Enroll(ctx context.Context, hubURL, token string, client *http.Client) (nodeID, credential string, err error) {
	hub, err := parseHubURL(hubURL)
	if err != nil {
		return "", "", err
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/api/nodes/enroll", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("enroll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("enroll: hub answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct{ NodeID, Credential string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("enroll: %w", err)
	}
	if out.NodeID == "" || out.Credential == "" {
		return "", "", fmt.Errorf("enroll: hub answered without node id or credential")
	}
	return out.NodeID, out.Credential, nil
}
