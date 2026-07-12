//go:build !linux

package main

// getDiskUsage is a no-op stub for non-Linux platforms (Windows dev).
// The disk monitor is only relevant in production (fnOS/Linux), so
// we skip the check entirely and never publish disk.warning events.
func getDiskUsage(path string) (total, free int64, err error) {
	return 0, 0, nil
}
