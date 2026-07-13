//go:build !darwin && !linux

package terminal

import "errors"

type terminalState struct{}

func getTerminalState(uintptr) (terminalState, error) {
	return terminalState{}, errors.New("raw mode is supported only on macOS and Linux")
}

func setTerminalState(uintptr, terminalState) error {
	return errors.New("raw mode is supported only on macOS and Linux")
}

func makeRawState(state terminalState) terminalState { return state }
