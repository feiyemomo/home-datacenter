//go:build !linux

package maintenance

import "errors"

// diskUsage is a stub for non-Linux builds (local Windows dev). The
// production deployment is a Linux Docker container; on other
// platforms the disk monitor simply logs the error and skips, keeping
// the rest of the maintenance loops functional.
func diskUsage(path string) (total, avail uint64, err error) {
	return 0, 0, errors.New("disk monitoring not supported on this platform")
}
