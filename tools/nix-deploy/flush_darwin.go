package main

import (
	"os"
	"syscall"
	"unsafe"
)

// flushInput discards input typed on the terminal and not yet read
// (TIOCFLUSH with FREAD).
func flushInput(f *os.File) {
	const tiocflush = 0x80047410
	which := int32(1)
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tiocflush, uintptr(unsafe.Pointer(&which)))
}
