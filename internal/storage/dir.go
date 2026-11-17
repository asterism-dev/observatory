package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	partialDir = ".partial"
	chunkSize  = 1 << 20
)

var clone = cloneFile

// Copies run outside Apply because a sparse copy of a large disk takes hours.
var (
	copiesMu sync.Mutex
	copies   = map[string]context.CancelFunc{} // disk file path -> cancel of its running copy
)

type Dir struct{ Path string }

// Apply creates missing disks marked Create (sparse, or reflink/sparse copy of CopyFrom), grows smaller ones, removes Deleted; never shrinks or removes anything else.
func (d Dir) Apply(disks []proto.StorageDisk, deleted []string) error {
	if err := os.MkdirAll(filepath.Join(d.Path, partialDir), 0o755); err != nil {
		return err
	}
	var errs []error
	for _, id := range deleted {
		if !plainName(id) {
			errs = append(errs, fmt.Errorf("delete %q: not a plain file name", id))
			continue
		}
		copiesMu.Lock()
		if cancel, ok := copies[d.File(id)]; ok {
			cancel()
		}
		if err := os.Remove(d.File(id)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", id, err))
		}
		copiesMu.Unlock()
	}
	for _, disk := range disks {
		if !plainName(disk.ID) || (disk.CopyFrom != "" && !plainName(disk.CopyFrom)) {
			errs = append(errs, fmt.Errorf("disk %q (copy from %q): not a plain file name", disk.ID, disk.CopyFrom))
			continue
		}
		if err := d.ensure(disk); err != nil {
			errs = append(errs, fmt.Errorf("disk %s: %w", disk.ID, err))
		}
	}
	return errors.Join(errs...)
}

// plainName rejects IDs that would resolve outside Path.
func plainName(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

// File is the path of a disk file.
func (d Dir) File(id string) string { return filepath.Join(d.Path, proto.StorageFileName(id)) }

func (d Dir) ensure(disk proto.StorageDisk) error {
	path := d.File(disk.ID)
	if st, err := os.Stat(path); err == nil {
		if st.Size() < disk.SizeBytes {
			return os.Truncate(path, disk.SizeBytes)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if !disk.Create {
		return nil // the hub shows the disk as missing
	}
	if disk.CopyFrom == "" {
		return d.build(context.Background(), path, nil, disk.SizeBytes)
	}

	copiesMu.Lock()
	defer copiesMu.Unlock()
	if _, running := copies[path]; running {
		return nil
	}
	src, err := os.Open(d.File(disk.CopyFrom))
	if os.IsNotExist(err) {
		return nil // source not there yet, retried on the next Apply
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	copies[path] = cancel
	go func() {
		defer src.Close()
		err := d.build(ctx, path, src, disk.SizeBytes)
		copiesMu.Lock()
		delete(copies, path)
		copiesMu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("disks: disk %s: copy: %v", disk.ID, err)
		}
	}()
	return nil
}

// build writes a sparse file of size at path, with src's data if src is set; a cancelled ctx leaves no file.
func (d Dir) build(ctx context.Context, path string, src *os.File, size int64) error {
	// nbdkit exports every file in the dir, so build elsewhere and rename.
	partial := filepath.Join(d.Path, partialDir, filepath.Base(path))
	dst, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if src != nil {
		if clone(dst, src) != nil {
			err = copySparse(ctx, dst, src, size)
		}
	}
	if err == nil {
		err = dst.Truncate(size)
	}
	// Without it a crash after the rename could leave a file with the copied data missing.
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	// Under copiesMu, so a delete either cancels before the rename or removes the renamed file.
	copiesMu.Lock()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Rename(partial, path)
	}
	copiesMu.Unlock()
	if err != nil {
		os.Remove(partial)
		return err
	}
	// The rename is only durable once the directory is synced; without it a crash could lose the new disk.
	return syncDir(d.Path)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// copySparse is the fallback for filesystems without reflink.
func copySparse(ctx context.Context, dst, src *os.File, limit int64) error {
	buf, zero := make([]byte, chunkSize), make([]byte, chunkSize)
	for off := int64(0); off < limit; off += chunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := buf[:min(int64(chunkSize), limit-off)]
		n, err := src.ReadAt(want, off)
		if err != nil && err != io.EOF {
			return err
		}
		if n > 0 && !bytes.Equal(want[:n], zero[:n]) {
			if _, werr := dst.WriteAt(want[:n], off); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
	}
	return nil
}

// Scan lists regular files directly in Path, sorted by name.
func (d Dir) Scan() ([]proto.StorageFile, error) {
	entries, err := os.ReadDir(d.Path)
	if err != nil {
		return nil, err
	}
	var out []proto.StorageFile
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			continue
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("stat %s: unexpected type %T", e.Name(), st.Sys())
		}
		out = append(out, proto.StorageFile{Name: e.Name(), SizeBytes: st.Size(), AllocatedBytes: sys.Blocks * 512})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
