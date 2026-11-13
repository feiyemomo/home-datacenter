//go:build !windows

package hostsec

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AuditSocketDirectory verifies that the Unix socket parent directory is secure:
// - Exists and is a directory
// - Is NOT a symbolic link
// - Permissions are <= 0755 (recommended 0750) and not world-writable
// - Owner UID is 0 (root) or matches current process UID
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
	perm := fi.Mode().Perm()
	if perm&0002 != 0 {
		return fmt.Errorf("socket directory %q is world-writable (mode %04o)", dir, perm)
	}
	if perm&^os.FileMode(0755) != 0 {
		return fmt.Errorf("socket directory %q has unsafe permissions %04o (must be <= 0755, recommended 0750)", dir, perm)
	}
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		currentUID := uint32(os.Getuid())
		if stat.Uid != 0 && stat.Uid != currentUID {
			return fmt.Errorf("socket directory %q owner UID %d untrusted (must be root (0) or helper process UID %d)", dir, stat.Uid, currentUID)
		}
	}
	return nil
}

// AuditSocketAncestry verifies the entire directory ancestry of sockPath:
// every ancestor directory up to root / must exist, be a directory, not be a symlink,
// not be world-writable, and be owned by root or the current process UID.
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
