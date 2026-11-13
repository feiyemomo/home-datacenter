//go:build windows

package hostsec

import (
	"fmt"
	"os"
	"path/filepath"
)

// AuditSocketDirectory verifies that the socket parent directory is secure.
func AuditSocketDirectory(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("lstat socket directory %q: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket directory path %q is not a directory", dir)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket directory %q is a symlink", dir)
	}
	return nil
}

// AuditSocketAncestry verifies all ancestor directories of sockPath on Windows.
func AuditSocketAncestry(sockPath string) error {
	absPath, err := filepath.Abs(sockPath)
	if err != nil {
		return fmt.Errorf("resolve absolute path for %q: %w", sockPath, err)
	}
	dir := filepath.Dir(absPath)

	for {
		if err := AuditSocketDirectory(dir); err != nil {
			return fmt.Errorf("ancestor directory %q failed audit: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}
