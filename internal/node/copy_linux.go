package node

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// copyContents copies only the data extents so holes in src stay holes in out.
func copyContents(out, in *os.File) error {
	st, err := in.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	var off int64
	for off < size {
		data, err := seek(in, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // only a hole is left
		}
		if err != nil {
			return plainCopy(out, in, off, err)
		}
		hole, err := seek(in, data, unix.SEEK_HOLE)
		if err != nil {
			return plainCopy(out, in, off, err)
		}
		if _, err := in.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(out, in, hole-data); err != nil {
			return err
		}
		off = hole
	}
	return out.Truncate(size)
}

func seek(f *os.File, off int64, whence int) (int64, error) {
	for {
		n, err := unix.Seek(int(f.Fd()), off, whence)
		if err != unix.EINTR {
			return n, err
		}
	}
}

// plainCopy is the fallback for filesystems without SEEK_DATA/SEEK_HOLE (ENXIO is handled by the caller).
func plainCopy(out, in *os.File, off int64, cause error) error {
	if !errors.Is(cause, unix.EINVAL) && !errors.Is(cause, unix.ENOTSUP) {
		return cause
	}
	if _, err := in.Seek(off, io.SeekStart); err != nil {
		return err
	}
	if _, err := out.Seek(off, io.SeekStart); err != nil {
		return err
	}
	_, err := io.Copy(out, in)
	return err
}
