//go:build !linux

package maintenance

import "errors"

// cpuUsage is a stub for non-Linux builds (local Windows dev). The
// production deployment is a Linux Docker container; on other
// platforms the resource monitor simply logs the error and skips.
func cpuUsage() (total, idle uint64, err error) {
	return 0, 0, errors.New("cpu monitoring not supported on this platform")
}

// memUsage is a stub for non-Linux builds.
func memUsage() (total, available uint64, err error) {
	return 0, 0, errors.New("memory monitoring not supported on this platform")
}
