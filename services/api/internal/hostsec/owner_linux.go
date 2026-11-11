//go:build linux

package hostsec

import (
	"os"
	"syscall"
)

func ExtractFileOwner(fi os.FileInfo) (uint32, uint32, bool) {
	if fi == nil {
		return 0, 0, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return st.Uid, st.Gid, true
	}
	return 0, 0, false
}
