//go:build windows

package hostsec

import (
	"fmt"
	"os"
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
