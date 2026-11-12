//go:build windows

package hostsec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	modKernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modKernel32.NewProc("LockFileEx")
	procUnlockFileEx = modKernel32.NewProc("UnlockFileEx")
)

const (
	winLockfileExclusiveLock = 0x00000002
)

// acquireCrossProcessFileLock acquires an intra-process mutex plus a Windows LockFileEx
// exclusive range lock on lockPath so multiple processes serialize state transitions safely.
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

	var ol syscall.Overlapped
	r1, _, callErr := procLockFileEx.Call(
		f.Fd(),
		uintptr(winLockfileExclusiveLock),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		_ = f.Close()
		pmu.Unlock()
		return nil, fmt.Errorf("LockFileEx %q: %w", cleanPath, callErr)
	}

	var released bool
	unlock := func() error {
		if released {
			return nil
		}
		released = true
		var errs []error
		var uol syscall.Overlapped
		r2, _, uCallErr := procUnlockFileEx.Call(
			f.Fd(),
			0,
			1,
			0,
			uintptr(unsafe.Pointer(&uol)),
		)
		if r2 == 0 {
			errs = append(errs, fmt.Errorf("UnlockFileEx %q: %w", cleanPath, uCallErr))
		}
		if cErr := f.Close(); cErr != nil {
			errs = append(errs, fmt.Errorf("close lock file %q: %w", cleanPath, cErr))
		}
		pmu.Unlock()
		return errors.Join(errs...)
	}
	return unlock, nil
}
