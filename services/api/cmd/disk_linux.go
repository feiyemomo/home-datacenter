//go:build linux

package main

import "golang.org/x/sys/unix"

// getDiskUsage returns the total and free bytes on the filesystem
// containing path. Uses unix.Statfs which is available on Linux.
func getDiskUsage(path string) (total, free int64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	// stat.Blocks * stat.Bsize = total bytes
	// stat.Bavail * stat.Bsize = bytes available to non-root
	total = int64(stat.Blocks) * int64(stat.Bsize)
	free = int64(stat.Bavail) * int64(stat.Bsize)
	return total, free, nil
}
