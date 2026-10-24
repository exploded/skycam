package alpaca

import "syscall"

// setSocketReuse marks a socket SO_REUSEADDR before bind, so several
// Alpaca servers can share the UDP discovery port, as the spec directs.
func setSocketReuse(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
