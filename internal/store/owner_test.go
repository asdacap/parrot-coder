package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOwnerChainClaimAndRelease(t *testing.T) {
	t.Parallel()
	state := t.TempDir()

	chain, ok, err := LoadOwnerChain(state, "host-a", "/w")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("empty chain reported an owner")
	}
	if err := chain.Claim(Owner{SessionID: "ses_1", WorkingDirectory: "/w", HostKey: "host-a", PID: 10}); err != nil {
		t.Fatal(err)
	}

	reloaded, ok, err := LoadOwnerChain(state, "host-a", "/w")
	if err != nil {
		t.Fatal(err)
	}
	owner, present := reloaded.Current()
	if !ok || !present || owner.SessionID != "ses_1" || owner.PID != 10 {
		t.Fatalf("current owner = %+v, want ses_1/pid 10", owner)
	}

	if err := reloaded.Release("host-a", "/w"); err != nil {
		t.Fatal(err)
	}
	after, _, err := LoadOwnerChain(state, "host-a", "/w")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := after.Current(); present {
		t.Fatal("released chain still reports an owner")
	}
}

// A chain is keyed by host as well as directory, so the same path on two
// machines never resolves to one record. This is what makes a foreign host's
// binding unreachable rather than merely ignored.
func TestOwnerChainIsolatesHosts(t *testing.T) {
	t.Parallel()
	state := t.TempDir()

	for _, host := range []string{"host-a", "host-b"} {
		chain, _, err := LoadOwnerChain(state, host, "/mnt/workspace/foo")
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.Claim(Owner{SessionID: "ses_" + host, WorkingDirectory: "/mnt/workspace/foo", HostKey: host, PID: 1}); err != nil {
			t.Fatal(err)
		}
	}

	for _, host := range []string{"host-a", "host-b"} {
		chain, ok, err := LoadOwnerChain(state, host, "/mnt/workspace/foo")
		if err != nil {
			t.Fatal(err)
		}
		owner, _ := chain.Current()
		if !ok || owner.SessionID != "ses_"+host {
			t.Fatalf("host %s resolved to %+v", host, owner)
		}
	}
}

// Two processes that read the same chain state and both publish must not both
// succeed: rename would let the second silently overwrite the first, so the
// loser has to be told.
func TestOwnerChainClaimDetectsConcurrentWriter(t *testing.T) {
	t.Parallel()
	state := t.TempDir()

	first, _, err := LoadOwnerChain(state, "host-a", "/w")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := LoadOwnerChain(state, "host-a", "/w")
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Claim(Owner{SessionID: "ses_first", WorkingDirectory: "/w", HostKey: "host-a", PID: 1}); err != nil {
		t.Fatal(err)
	}
	err = second.Claim(Owner{SessionID: "ses_second", WorkingDirectory: "/w", HostKey: "host-a", PID: 2})
	if !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("second claim error = %v, want ErrOwnerConflict", err)
	}

	reloaded, _, err := LoadOwnerChain(state, "host-a", "/w")
	if err != nil {
		t.Fatal(err)
	}
	if owner, _ := reloaded.Current(); owner.SessionID != "ses_first" {
		t.Fatalf("winning claim was overwritten: %+v", owner)
	}
}

// Concurrent claimers must never lose a write: each one that succeeds occupies
// a version of its own, and each one that fails is told so.
//
// The count of winners is deliberately not asserted. A claimer that loads the
// chain after another's claim has landed sees the newer version and may claim
// the next one, so more than one can legitimately succeed. What must hold is
// that no two claims occupy the same version, which is exactly what rename
// would violate and link does not.
func TestOwnerChainConcurrentClaimsNeverLoseAWrite(t *testing.T) {
	t.Parallel()
	state := t.TempDir()

	const claimers = 8
	var wg sync.WaitGroup
	results := make([]error, claimers)
	for i := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chain, _, err := LoadOwnerChain(state, "host-a", "/w")
			if err != nil {
				results[i] = err
				return
			}
			results[i] = chain.Claim(Owner{SessionID: "ses", WorkingDirectory: "/w", HostKey: "host-a", PID: i + 1})
		}()
	}
	wg.Wait()

	winners := 0
	for _, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrOwnerConflict):
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if winners == 0 {
		t.Fatal("every claimer lost; the chain made no progress")
	}

	entries, err := os.ReadDir(filepath.Join(OwnersRoot(state), ownerKey("host-a", "/w")))
	if err != nil {
		t.Fatal(err)
	}
	versions := make(map[int]bool)
	for _, entry := range entries {
		version, valid := ownerVersionOf(entry.Name())
		if !valid {
			// A loser must not leave its staged file behind.
			t.Fatalf("chain contains stray file %q", entry.Name())
		}
		if versions[version] {
			t.Fatalf("version %d was written twice", version)
		}
		versions[version] = true
	}
	// One record per winner: no claim was overwritten by another.
	if len(versions) != winners {
		t.Fatalf("%d records for %d winning claims", len(versions), winners)
	}
	for version := 1; version <= winners; version++ {
		if !versions[version] {
			t.Fatalf("chain is not contiguous: version %d is missing", version)
		}
	}
}
