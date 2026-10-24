//go:build linux && (arm64 || amd64)

package asisdk

import "github.com/ebitengine/purego"

// clong is C long: 8 bytes on 64-bit Linux (LP64).
type clong = int64

func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}
