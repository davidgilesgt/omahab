package system

import "syscall"

func statDisk(path string) (total, used, free uint64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return
	}

	total = stat.Blocks * uint64(stat.Bsize)
	free = stat.Bfree * uint64(stat.Bsize)
	used = total - free
	return
}
