//go:build !windows

package hostsec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireCrossProcessFileLock acquires an intra-process mutex plus a kernel advisory exclusive
// flock on lockPath so multiple helper/bridge processes serialize state transitions safely.
func acquireCrossProcessFileLock(lockPath string) (func() error, error) {
	cleanPath := filepath.Clean(lockPath)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0700); err != nil {
		return nil, fmt.Errorf("create lock directory %q: %w", filepath.Dir(cleanPath), err)
	}

	pmu := getProcessMutex(cleanPath)
	pmu.Lock()

	f, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		pmu.Unlock()
		return nil, fmt.Errorf("open lock file %q: %w", cleanPath, err)
	}

	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		_ = f.Close()
		pmu.Unlock()
		return nil, fmt.Errorf("flock LOCK_EX on %q: %w", cleanPath, err)
	}

	var released bool
	unlock := func() error {
		if released {
			return nil
		}
		released = true
		var errs []error
		if uErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); uErr != nil {
			errs = append(errs, fmt.Errorf("flock LOCK_UN %q: %w", cleanPath, uErr))
		}
		if cErr := f.Close(); cErr != nil {
			errs = append(errs, fmt.Errorf("close lock file %q: %w", cleanPath, cErr))
		}
		pmu.Unlock()
		return errors.Join(errs...)
	}
	return unlock, nil
}
