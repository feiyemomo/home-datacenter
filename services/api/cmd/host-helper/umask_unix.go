//go:build !windows

package main

import "syscall"

func setSocketUmask() func() {
	old := syscall.Umask(0007)
	return func() {
		syscall.Umask(old)
	}
}
