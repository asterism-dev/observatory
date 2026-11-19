//go:build !linux

package ociimage

import (
	"context"
	"errors"
)

func (s Store) Disk(ctx context.Context, digest string) (string, error) {
	return "", errors.New("root disks are only built on linux")
}
