//go:build !linux

package storage

import (
	"errors"
	"os"
)

func cloneFile(dst, src *os.File) error { return errors.ErrUnsupported }
