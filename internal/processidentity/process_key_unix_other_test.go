//go:build unix && !linux

package processidentity

import "testing"

func TestInspectProcessDetectsMissingPID(t *testing.T) {
	if got := inspectProcess(1<<30, "pid:1073741824"); got != LivenessDead {
		t.Fatalf("inspectProcess(missing PID) = %v, want dead", got)
	}
}
