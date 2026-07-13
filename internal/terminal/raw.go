package terminal

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// RawState restores a terminal to the state it had before EnableRawMode.
// Close is safe to call more than once.
type RawState struct {
	file  *os.File
	state terminalState
	once  sync.Once
	err   error
}

// EnableRawMode switches file to raw character-at-a-time input mode.
func EnableRawMode(file *os.File) (*RawState, error) {
	if file == nil {
		return nil, errors.New("terminal: raw mode requires a file")
	}

	state, err := getTerminalState(file.Fd())
	if err != nil {
		return nil, fmt.Errorf("terminal: get terminal state: %w", err)
	}
	raw := makeRawState(state)
	if err := setTerminalState(file.Fd(), raw); err != nil {
		// Some kernels and pseudo terminals may partially apply an ioctl before
		// reporting failure, so always attempt restoration.
		restoreErr := setTerminalState(file.Fd(), state)
		return nil, errors.Join(fmt.Errorf("terminal: enable raw mode: %w", err), restoreErr)
	}
	return &RawState{file: file, state: state}, nil
}

// Close restores the original terminal state.
func (r *RawState) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		if r.file != nil {
			r.err = setTerminalState(r.file.Fd(), r.state)
			if r.err != nil {
				r.err = fmt.Errorf("terminal: restore terminal state: %w", r.err)
			}
		}
	})
	return r.err
}
