//go:build !linux

package hostsec

import "os"

func ExtractFileOwner(fi os.FileInfo) (uint32, uint32, bool) {
	return 0, 0, false
}
