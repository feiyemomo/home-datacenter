package hostsec

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	processLocksMu sync.Mutex
	processLocks   = make(map[string]*sync.Mutex)
)

func getProcessMutex(cleanPath string) *sync.Mutex {
	processLocksMu.Lock()
	defer processLocksMu.Unlock()
	m, ok := processLocks[cleanPath]
	if !ok {
		m = &sync.Mutex{}
		processLocks[cleanPath] = m
	}
	return m
}

// writeFileAtomicFailClosed writes data to targetPath via exclusive temp file + fsync + atomic rename.
// All I/O errors fail closed and never leave partially written temp files behind.
func writeFileAtomicFailClosed(targetPath string, data []byte, perm os.FileMode) error {
	cleanTarget := filepath.Clean(targetPath)
	parentDir := filepath.Dir(cleanTarget)
	baseName := filepath.Base(cleanTarget)
	tmpFile, err := os.CreateTemp(parentDir, fmt.Sprintf(".%s.tmp.*", baseName))
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := func() {
		_ = os.Remove(tmpPath)
	}
	if perm != 0 && perm != 0600 {
		if chErr := tmpFile.Chmod(perm); chErr != nil {
			_ = tmpFile.Close()
			cleanupTmp()
			return chErr
		}
	}

	written := 0
	for written < len(data) {
		n, wErr := tmpFile.Write(data[written:])
		written += n
		if wErr != nil {
			_ = tmpFile.Close()
			cleanupTmp()
			return wErr
		}
		if n == 0 {
			_ = tmpFile.Close()
			cleanupTmp()
			return os.ErrInvalid
		}
	}
	if sErr := tmpFile.Sync(); sErr != nil {
		_ = tmpFile.Close()
		cleanupTmp()
		return sErr
	}
	if cErr := tmpFile.Close(); cErr != nil {
		cleanupTmp()
		return cErr
	}
	if rErr := os.Rename(tmpPath, cleanTarget); rErr != nil {
		cleanupTmp()
		return rErr
	}
	// Directory fsync to ensure directory entry durability across crashes.
	// On POSIX systems, directory sync is strictly required. On Windows,
	// syncing directory handles is not supported by the kernel and returns an error.
	if d, dErr := os.Open(parentDir); dErr != nil {
		if runtime.GOOS != "windows" {
			return fmt.Errorf("failed to open parent directory for sync: %w", dErr)
		}
	} else {
		sErr := d.Sync()
		cErr := d.Close()
		if runtime.GOOS != "windows" {
			if sErr != nil {
				return fmt.Errorf("failed to sync parent directory: %w", sErr)
			}
			if cErr != nil {
				return fmt.Errorf("failed to close parent directory: %w", cErr)
			}
		}
	}
	return nil
}

// removeFileFailClosed removes path and fails closed on any error other than os.IsNotExist.
func removeFileFailClosed(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
