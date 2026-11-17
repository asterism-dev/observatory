//go:build !linux

package node

func totalMemoryMB() int { return 0 }
