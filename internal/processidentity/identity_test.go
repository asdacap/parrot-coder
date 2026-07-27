package processidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQualifiedHostKeyIncludesHostnameAndStableKey(t *testing.T) {
	hostname, _ := os.Hostname()
	got := qualifiedHostKey("  key-value\n")
	if got != strings.TrimSpace(hostname)+":key-value" {
		t.Fatalf("qualified host key = %q", got)
	}
}

// Every host key is hostname-qualified, including one derived from a machine
// ID. Hosts cloned from an image, and containers sharing the host's
// /etc/machine-id, report identical machine IDs; without the hostname they
// would share a host key and so claim each other's sessions.
func TestLoadQualifiesMachineIDDerivedHostKey(t *testing.T) {
	identity, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	if prefix := strings.TrimSpace(hostname) + ":"; !strings.HasPrefix(identity.HostKey, prefix) {
		t.Fatalf("host key %q is not qualified with %q", identity.HostKey, prefix)
	}
}

func TestLoadReturnsCurrentPIDAndStableHostKey(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.PID != os.Getpid() || second.PID != os.Getpid() || first.HostKey == "" || first.ProcessKey == "" || first != second {
		t.Fatalf("identities = %#v %#v", first, second)
	}
	// Systems with a machine ID do not need the fallback file. When created,
	// it must remain private.
	if info, err := os.Stat(filepath.Join(dir, hostKeyFile)); err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("fallback host key mode = %o", info.Mode().Perm())
	}
}

func TestInspectOwnershipConservatively(t *testing.T) {
	local, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		owner Identity
		want  Liveness
	}{
		{name: "current process", owner: local, want: LivenessAlive},
		{name: "legacy ownerless", owner: Identity{}, want: LivenessDead},
		{name: "partial owner", owner: Identity{HostKey: local.HostKey, PID: local.PID}, want: LivenessUnknown},
		{name: "foreign host", owner: Identity{HostKey: "foreign", PID: local.PID, ProcessKey: local.ProcessKey}, want: LivenessUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Inspect(local, test.owner); got != test.want {
				t.Fatalf("Inspect(%#v) = %v, want %v", test.owner, got, test.want)
			}
		})
	}
}
