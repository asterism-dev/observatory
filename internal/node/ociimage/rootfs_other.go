//go:build !linux

package ociimage

import (
	"context"
	"errors"
)

func (s Store) Disk(ctx context.Context, digest string) (string, error) {
	return "", errors.New("root disks are only built on linux")
}

var fsSpace = statfsSpace

func statfsSpace(string) (int64, int64, error) {
	return 0, 0, errors.New("filesystem space is only read on linux")
}
