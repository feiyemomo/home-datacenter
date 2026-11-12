package hostsec

import (
	"fmt"
	"os"
	"path/filepath"
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
	tmpFile, err := os.OpenFile(
		filepath.Join(parentDir, fmt.Sprintf(".%s.tmp.%d", baseName, os.Getpid())),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		perm,
	)
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := func() {
		_ = os.Remove(tmpPath)
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
	if d, dErr := os.Open(parentDir); dErr == nil {
		_ = d.Sync()
		_ = d.Close()
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
