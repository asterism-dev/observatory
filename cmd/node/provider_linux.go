package main

import (
	"fmt"
	"os"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/provider/firecracker"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

func newProvider(cfg node.Config) (node.Provider, error) {
	switch cfg.Provider {
	case "firecracker":
		return firecracker.New(cfg)
	default:
		return nil, fmt.Errorf("unknown provider %q for this OS", cfg.Provider)
	}
}

func newEgress(cfg node.Config) (node.EgressHost, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	e, err := linuxvm.NewEgress(cfg.Paths.VMs, bin)
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	return e, nil
}

func dataDir(string) string { return "/var/lib/observatory-node" }
