//go:build linux

package processidentity

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processKey is the kernel process start tick. Together with PID it remains
// stable for one process lifetime and changes when the PID is reused.
func processKey(pid int) (string, error) {
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// comm is parenthesized and may contain spaces or parentheses. The fields
	// after its final ')' start at field 3; starttime is field 22.
	close := strings.LastIndexByte(string(raw), ')')
	if close < 0 {
		return "", fmt.Errorf("process identity: malformed stat for PID %d", pid)
	}
	fields := strings.Fields(string(raw[close+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("process identity: malformed stat for PID %d", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("process identity: malformed start time for PID %d: %w", pid, err)
	}
	return strings.TrimSpace(string(bootID)) + ":" + fields[19], nil
}

func inspectProcess(pid int, expected string) Liveness {
	key, err := processKey(pid)
	if os.IsNotExist(err) {
		return LivenessDead
	}
	if err != nil {
		return LivenessUnknown
	}
	if key != expected {
		return LivenessDead
	}
	return LivenessAlive
}
