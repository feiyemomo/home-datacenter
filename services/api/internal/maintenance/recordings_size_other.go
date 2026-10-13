//go:build !linux

package maintenance

import "os"

// fileDeviceAndInode returns false on non-Linux systems where Stat_t is unavailable.
func fileDeviceAndInode(info os.FileInfo) (uint64, uint64, bool) {
	return 0, 0, false
}

// allocatedSize returns the file size on non-Linux systems where Stat_t is unavailable.
func allocatedSize(info os.FileInfo) uint64 {
	return uint64(info.Size())
}
