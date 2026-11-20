package node

import "golang.org/x/sys/unix"

// freeBytes counts in Frsize: Linux's Bsize is the preferred I/O size, not the unit of Bavail.
func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * st.Frsize, nil
}

func filesystemUsage(dir string) (used, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * st.Frsize
	return total - int64(st.Bfree)*st.Frsize, total, nil
}
