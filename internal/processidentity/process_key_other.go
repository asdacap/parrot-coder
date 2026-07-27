//go:build !linux

package processidentity

import (
	"fmt"
	"os"
)

// Platforms without a queryable process start identity conservatively use the
// PID. Inspect consequently retains an owner while that PID exists.
func processKey(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("process identity: invalid PID %d", pid)
	}
	if pid != os.Getpid() && !Alive(pid) {
		return "", fmt.Errorf("process identity: PID %d is not alive", pid)
	}
	return fmt.Sprintf("pid:%d", pid), nil
}

func inspectProcess(pid int, expected string) Liveness {
	if !Alive(pid) {
		return LivenessDead
	}
	key, err := processKey(pid)
	if err != nil {
		return LivenessDead
	}
	if key == expected {
		return LivenessUnknown // A reused PID is indistinguishable here.
	}
	return LivenessDead
}
