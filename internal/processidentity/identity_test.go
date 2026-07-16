package processidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFallbackHostKeyIncludesHostnameAndStableKey(t *testing.T) {
	hostname, _ := os.Hostname()
	got := fallbackHostKey("  key-value\n")
	if got != strings.TrimSpace(hostname)+":key-value" {
		t.Fatalf("fallback host key = %q", got)
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
	if first.PID != os.Getpid() || second.PID != os.Getpid() || first.HostKey == "" || first.HostKey != second.HostKey {
		t.Fatalf("identities = %#v %#v", first, second)
	}
	// Systems with a machine ID do not need the fallback file. When created,
	// it must remain private.
	if info, err := os.Stat(filepath.Join(dir, hostKeyFile)); err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("fallback host key mode = %o", info.Mode().Perm())
	}
}
