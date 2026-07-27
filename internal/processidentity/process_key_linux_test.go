//go:build linux

package processidentity

import (
	"os"
	"testing"
)

func TestInspectDetectsMissingAndReusedLocalPID(t *testing.T) {
	local, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		owner Identity
	}{
		{name: "reused current PID", owner: Identity{HostKey: local.HostKey, PID: os.Getpid(), ProcessKey: local.ProcessKey + "-old"}},
		{name: "missing PID", owner: Identity{HostKey: local.HostKey, PID: 1 << 30, ProcessKey: "old"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Inspect(local, test.owner); got != LivenessDead {
				t.Fatalf("Inspect(%#v) = %v, want dead", test.owner, got)
			}
		})
	}
}
