//go:build windows && amd64

package asisdk

import "syscall"

// clong is C long: 4 bytes on 64-bit Windows (LLP64).
type clong = int32

func openLibrary(path string) (uintptr, error) {
	h, err := syscall.LoadLibrary(path)
	return uintptr(h), err
}
