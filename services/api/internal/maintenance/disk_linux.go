//go:build linux

package maintenance

import "golang.org/x/sys/unix"

// diskUsage returns the total and available bytes on the filesystem
// containing path, using the Linux statfs syscall. This is the real
// implementation used by the production Docker container.
func diskUsage(path string) (total, avail uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}
