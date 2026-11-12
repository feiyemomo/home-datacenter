//go:build windows

package main

func setSocketUmask() func() {
	return func() {}
}
