//go:build darwin || linux

package terminal

import "golang.org/x/sys/unix"

type terminalState = *unix.Termios

func makeRawState(state terminalState) terminalState {
	raw := *state
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	// Keep output post-processing enabled. Parrot needs raw input, but committed
	// transcript newlines should retain normal terminal carriage-return behavior.
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	// A short driver-level timeout lets the decoder distinguish a standalone
	// Escape from the prefix of an escape sequence without a helper goroutine.
	raw.Cc[unix.VMIN] = 0
	raw.Cc[unix.VTIME] = 1
	return &raw
}
