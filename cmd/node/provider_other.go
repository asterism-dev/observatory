//go:build !linux

package main

import (
	"fmt"
	"path/filepath"

	"github.com/asterism-dev/observatory/internal/node"
)

func newProvider(cfg node.Config) (node.Provider, error) {
	return nil, fmt.Errorf("unknown provider %q for this OS", cfg.Provider)
}

func newEgress(node.Config) (node.EgressHost, error) { return nil, nil }

func dataDir(home string) string { return filepath.Join(home, ".observatory-node") }
