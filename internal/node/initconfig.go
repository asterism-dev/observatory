package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// InitOptions are the node.json fields enrollment sets; a zero MemoryMB leaves capacity as it is.
type InitOptions struct {
	Path, HubURL, NodeID, Credential string
	MemoryMB                         int
}

// InitConfig creates node.json or updates only the given fields, keeping every other key (paths, provider sections) as written.
func InitConfig(o InitOptions, vmsDir, imagesDir string) error {
	connect, err := connectURL(o.HubURL)
	if err != nil {
		return err
	}
	if o.Credential == "" || o.NodeID == "" {
		return errors.New("node id and credential are required")
	}
	doc := map[string]json.RawMessage{}
	b, err := os.ReadFile(o.Path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", o.Path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := setJSON(doc, "paths", map[string]string{"vms": vmsDir, "images": imagesDir}); err != nil {
			return err
		}
	default:
		return err
	}
	for _, set := range []func() error{
		func() error { return setJSON(doc, "nodeId", o.NodeID) },
		func() error { return setJSON(doc, "provider", "firecracker") },
		func() error { return setNested(doc, "hub", map[string]any{"url": connect, "credential": o.Credential}) },
		func() error {
			if o.MemoryMB == 0 {
				return nil
			}
			return setNested(doc, "capacity", map[string]any{"memoryMB": o.MemoryMB})
		},
	} {
		if err := set(); err != nil {
			return err
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o755); err != nil {
		return err
	}
	// writeFileAtomic creates the file 0600: node.json holds the credential.
	return writeFileAtomic(o.Path, append(out, '\n'))
}

// CheckEnrollMemory fails before the one-time token is redeemed when the resulting node.json could not start the node.
func CheckEnrollMemory(path string, memoryMB int) error {
	if memoryMB > 0 {
		return nil
	}
	if _, err := LoadConfig(path); err == nil {
		return nil
	}
	_, err := defaultMemoryMB(totalMemoryMB())
	return err
}

// connectURL turns the hub's http(s)://host:port into the node's websocket URL.
func connectURL(hub string) (string, error) {
	u, err := url.Parse(hub)
	if err != nil || u.Host == "" || u.Port() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("hub %q: want http://host:port", hub)
	}
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + u.Host + "/api/nodes/connect", nil
}

func setJSON(doc map[string]json.RawMessage, key string, v any) error {
	b, err := json.Marshal(v)
	doc[key] = b
	return err
}

// setNested sets keys inside the object doc[key], creating it if needed and keeping its other keys.
func setNested(doc map[string]json.RawMessage, key string, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	obj := map[string]json.RawMessage{}
	if raw, ok := doc[key]; ok {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	for k, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		obj[k] = b
	}
	return setJSON(doc, key, obj)
}

// ReadSecret reads one secret from r (the installer pipes the enrollment token in so it never appears on a command line).
func ReadSecret(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("nothing on stdin")
	}
	return t, nil
}
