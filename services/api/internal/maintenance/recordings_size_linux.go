//go:build linux

package maintenance

import (
	"os"
	"syscall"
)

// fileDeviceAndInode returns the (dev, ino) tuple on Linux to identify hardlinks.
func fileDeviceAndInode(info os.FileInfo) (uint64, uint64, bool) {
	if sys := info.Sys(); sys != nil {
		if st, ok := sys.(*syscall.Stat_t); ok {
			return uint64(st.Dev), uint64(st.Ino), true
		}
	}
	return 0, 0, false
}

// allocatedSize returns the real physical/allocated bytes on disk for a file.
// On Linux filesystems (including Btrfs with transparent compression, sparse files,
// or deduplicated extents), Stat_t.Blocks contains the number of 512B blocks allocated.
func allocatedSize(info os.FileInfo) uint64 {
	if sys := info.Sys(); sys != nil {
		if st, ok := sys.(*syscall.Stat_t); ok {
			return uint64(st.Blocks) * 512
		}
	}
	return uint64(info.Size())
}
