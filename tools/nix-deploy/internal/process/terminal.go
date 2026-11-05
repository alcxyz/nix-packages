package process

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

// Attached commands such as sudo must retain terminal input despite their
// separate process group. Serialize terminal ownership and restore it on wait.
var terminalLock sync.Mutex

func attachTerminal(cmd *exec.Cmd) func() {
	input, ok := cmd.Stdin.(*os.File)
	if !ok {
		return func() {}
	}
	terminalLock.Lock()
	var previous int32
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, input.Fd(), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&previous)))
	if err != 0 || int(previous) != syscall.Getpgrp() {
		// A background caller must not move itself or its child into the
		// foreground. Commands may run there until they actually read input.
		terminalLock.Unlock()
		return func() {}
	}
	// Keep a small foreground supervisor alive to relay terminal Ctrl-C.
	// SSH can translate SIGINT to status 255, so exit codes cannot identify it.
	bash, lookupErr := exec.LookPath("bash")
	if lookupErr != nil {
		cmd.Err = fmt.Errorf("terminal signal relay requires bash: %w", lookupErr)
		terminalLock.Unlock()
		return func() {}
	}
	originalPath := cmd.Path
	originalArgs := append([]string(nil), cmd.Args[1:]...)
	cmd.Path = bash
	cmd.Args = append([]string{"bash", "-c", `controller=$1
shift
trap 'trap "" INT; kill -INT "$controller"; wait "$child"; exit 130' INT
trap 'wait "$child"; exit 143' TERM
"$@" <&0 &
child=$!
wait "$child"
exit $?
`, "nixfleet-command", fmt.Sprint(os.Getpid()), originalPath}, originalArgs...)
	cmd.SysProcAttr.Foreground = true
	cmd.SysProcAttr.Ctty = int(input.Fd())
	return func() {
		// Restoring the caller's group is a terminal write from the background.
		ignored := signal.Ignored(syscall.SIGTTOU)
		signal.Ignore(syscall.SIGTTOU)
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, input.Fd(), syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&previous)))
		if !ignored {
			signal.Reset(syscall.SIGTTOU)
		}
		terminalLock.Unlock()
	}
}
