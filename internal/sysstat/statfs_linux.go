package sysstat

import "golang.org/x/sys/unix"

func FilesystemUsage(dir string) (used, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * st.Frsize
	return total - int64(st.Bfree)*st.Frsize, total, nil
}
