package main

import (
	"os"
	"syscall"
)

// flushInput discards input typed on the terminal and not yet read
// (TCFLSH with TCIFLUSH; the value of x86, arm and riscv, the fleet's
// architectures).
func flushInput(f *os.File) {
	const tcflsh, tciflush = 0x540B, 0
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tcflsh, tciflush)
}
