//go:build !linux

package node

import (
	"io"
	"os"
)

func copyContents(out, in *os.File) error {
	_, err := io.Copy(out, in)
	return err
}
