//go:build linux

package terminal

import "golang.org/x/sys/unix"

func getTerminalState(fd uintptr) (terminalState, error) {
	return unix.IoctlGetTermios(int(fd), unix.TCGETS)
}

func setTerminalState(fd uintptr, state terminalState) error {
	return unix.IoctlSetTermios(int(fd), unix.TCSETS, state)
}
