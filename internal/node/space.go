package node

import (
	"fmt"

	"golang.org/x/sys/unix"
)

var statfsFree = func(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func checkSpace(dir string, need int64) error {
	free, err := statfsFree(dir)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("not enough space on node: %d MiB free, %d MiB needed", free>>20, need>>20)
	}
	return nil
}
