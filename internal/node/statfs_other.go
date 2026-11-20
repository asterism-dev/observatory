//go:build unix && !linux

package node

import "golang.org/x/sys/unix"

func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func filesystemUsage(dir string) (used, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	return total - int64(st.Bfree)*int64(st.Bsize), total, nil
}
