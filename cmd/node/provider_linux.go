package main

import (
	"fmt"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/provider/firecracker"
)

func newProvider(cfg node.Config) (node.Provider, error) {
	switch cfg.Provider {
	case "firecracker":
		return firecracker.New(cfg)
	default:
		return nil, fmt.Errorf("unknown provider %q for this OS", cfg.Provider)
	}
}

func dataDir(string) string { return "/var/lib/vm-manager" }
