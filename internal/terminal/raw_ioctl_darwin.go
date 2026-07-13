//go:build darwin

package terminal

import "golang.org/x/sys/unix"

func getTerminalState(fd uintptr) (terminalState, error) {
	return unix.IoctlGetTermios(int(fd), unix.TIOCGETA)
}

func setTerminalState(fd uintptr, state terminalState) error {
	return unix.IoctlSetTermios(int(fd), unix.TIOCSETA, state)
}
