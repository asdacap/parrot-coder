//go:build darwin || linux

package terminal

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestMakeRawState(t *testing.T) {
	state := &unix.Termios{
		Iflag: unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON,
		Oflag: unix.OPOST,
		Lflag: unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG,
	}
	raw := makeRawState(state)
	if raw == state {
		t.Fatal("makeRawState mutated the saved state")
	}
	if raw.Iflag != 0 || raw.Oflag != unix.OPOST || raw.Lflag != 0 || raw.Cflag&unix.CS8 == 0 {
		t.Fatalf("raw flags = %#v", raw)
	}
	if raw.Cc[unix.VMIN] != 0 || raw.Cc[unix.VTIME] != 1 {
		t.Fatalf("raw control characters = %#v", raw.Cc)
	}
}
