//go:build !windows

package alpaca

import "syscall"

// setSocketReuse marks a socket SO_REUSEADDR before bind — see the Windows
// variant for why.
func setSocketReuse(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
