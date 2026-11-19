//go:build !linux

package main

import "github.com/asterism-dev/observatory/internal/node"

func newUpdater(string) node.Updater { return nil }
