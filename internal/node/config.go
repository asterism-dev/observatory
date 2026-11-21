package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	NodeID string `json:"nodeId"`
	Hub    struct {
		URL        string `json:"url"`
		Credential string `json:"credential"`
	} `json:"hub"`
	Provider string `json:"provider"`
	Paths    struct {
		VMs    string `json:"vms"`
		Images string `json:"images"`
	} `json:"paths"`
	Capacity struct {
		CPUs     int `json:"cpus"`
		MemoryMB int `json:"memoryMB"`
	} `json:"capacity"`
	Raw map[string]json.RawMessage `json:"-"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c.Raw); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	for field, v := range map[string]string{
		"hub.url": c.Hub.URL, "hub.credential": c.Hub.Credential, "provider": c.Provider,
		"paths.vms": c.Paths.VMs, "paths.images": c.Paths.Images,
	} {
		if v == "" {
			return c, fmt.Errorf("config: %s is required", field)
		}
	}
	if c.Hub.URL, err = parseHubURL(c.Hub.URL); err != nil {
		return c, fmt.Errorf("config: hub.url: %w", err)
	}
	home, _ := os.UserHomeDir()
	c.Paths.VMs, c.Paths.Images = expandHome(c.Paths.VMs, home), expandHome(c.Paths.Images, home)
	if c.NodeID == "" {
		c.NodeID, _ = os.Hostname()
	}
	if c.Capacity.CPUs == 0 {
		c.Capacity.CPUs = runtime.NumCPU()
	}
	if c.Capacity.MemoryMB == 0 {
		mb, err := defaultMemoryMB(totalMemoryMB())
		if err != nil {
			return c, err
		}
		c.Capacity.MemoryMB = mb
	}
	return c, nil
}

const hostReserveMB = 4096

func defaultMemoryMB(totalMB int) (int, error) {
	if totalMB == 0 {
		return 0, errors.New("config: capacity.memoryMB is required on this platform")
	}
	if totalMB <= hostReserveMB {
		return 0, fmt.Errorf("config: this host has %d MB of RAM and %d MB stay with the host; set capacity.memoryMB (installer: --memory-mb N)", totalMB, hostReserveMB)
	}
	return totalMB - hostReserveMB, nil
}

// Public returns the config file as written, minus the hub credential, for display on the hub.
func (c Config) Public() (json.RawMessage, error) {
	raw := maps.Clone(c.Raw)
	if hub, ok := raw["hub"]; ok {
		var h map[string]json.RawMessage
		if err := json.Unmarshal(hub, &h); err != nil {
			return nil, err
		}
		delete(h, "credential")
		b, err := json.Marshal(h)
		if err != nil {
			return nil, err
		}
		raw["hub"] = b
	}
	return json.Marshal(raw)
}

// ProviderSection decodes the config block named after the provider; a missing block leaves v untouched.
func (c Config) ProviderSection(v any) error {
	raw, ok := c.Raw[c.Provider]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func expandHome(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[1:])
	}
	return p
}
