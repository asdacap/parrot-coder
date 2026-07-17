package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/atomicfile"
)

const ownersDir = "owners"

// OwnerVersion is the schema version of an owner record.
const OwnerVersion = 1

// ErrOwnerConflict reports that another process published a claim first and the
// caller must re-read and decide again.
var ErrOwnerConflict = errors.New("store: interactive owner changed concurrently")

// Owner binds one working directory on one machine to a session.
type Owner struct {
	Version          int    `json:"version"`
	SessionID        string `json:"session_id"`
	WorkingDirectory string `json:"working_directory"`
	HostKey          string `json:"host_key"`
	PID              int    `json:"pid"`
	ClaimedAt        string `json:"claimed_at"`
}

// OwnerChain is an append-only sequence of owner records for one
// (host, working directory) pair. The highest version is current.
//
// A chain is keyed by host as well as directory because a working directory is
// a host-local name: /mnt/workspace/foo on one machine and the same path on
// another are different directories on different disks. Records from another
// host describe a directory this host cannot see, so they are never read to
// make a decision and never written.
type OwnerChain struct {
	dir     string
	version int
	current Owner
}

// ownerKey derives a path-safe directory name from a host and working
// directory.
func ownerKey(hostKey, workingDirectory string) string {
	sum := sha256.Sum256([]byte(hostKey + "\x00" + workingDirectory))
	return hex.EncodeToString(sum[:])
}

// OwnersRoot is the directory holding every owner chain.
func OwnersRoot(state string) string { return filepath.Join(state, ownersDir) }

// LoadOwnerChain reads the current owner for a host and working directory. The
// returned chain reports whether a record exists via ok.
func LoadOwnerChain(state, hostKey, workingDirectory string) (chain *OwnerChain, ok bool, err error) {
	dir := filepath.Join(OwnersRoot(state), ownerKey(hostKey, workingDirectory))
	result := &OwnerChain{dir: dir}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read owner chain: %w", err)
	}

	versions := make([]int, 0, len(entries))
	for _, entry := range entries {
		if version, valid := ownerVersionOf(entry.Name()); valid {
			versions = append(versions, version)
		}
	}
	if len(versions) == 0 {
		return result, false, nil
	}
	sort.Ints(versions)

	// Read newest first: an older record is only a fallback if the newest is
	// unreadable, which happens if a writer died between link and this read.
	for i := len(versions) - 1; i >= 0; i-- {
		var owner Owner
		err := atomicfile.ReadJSON(filepath.Join(dir, ownerFileName(versions[i])), &owner)
		if err == nil && owner.Version == OwnerVersion {
			result.version = versions[i]
			result.current = owner
			return result, true, nil
		}
	}
	result.version = versions[len(versions)-1]
	return result, false, nil
}

func ownerFileName(version int) string { return "v" + strconv.Itoa(version) + ".json" }

func ownerVersionOf(name string) (int, bool) {
	if !strings.HasPrefix(name, "v") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "v"), ".json"))
	if err != nil || version <= 0 {
		return 0, false
	}
	return version, true
}

// Current returns the current owner record and whether one exists.
func (c *OwnerChain) Current() (Owner, bool) { return c.current, c.current.SessionID != "" }

// Claim publishes owner as the next record in the chain, reporting
// [ErrOwnerConflict] if another process published first.
//
// The write is a link onto a version-named target rather than a rename onto a
// fixed name. Rename would replace whatever a concurrent claimer had just
// published, losing that claim with no error; link refuses and reports the
// conflict, which is what makes the caller's read-decide-write cycle a genuine
// compare-and-swap. It needs no lock manager, so it stays correct on a mount
// that grants advisory locks locally without telling other hosts.
func (c *OwnerChain) Claim(owner Owner) error {
	owner.Version = OwnerVersion
	if owner.ClaimedAt == "" {
		owner.ClaimedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("store: create owner chain: %w", err)
	}
	err := atomicfile.Link(filepath.Join(c.dir, ownerFileName(c.version+1)), owner)
	if errors.Is(err, fs.ErrExist) {
		return ErrOwnerConflict
	}
	if err != nil {
		return fmt.Errorf("store: publish owner: %w", err)
	}
	c.version++
	c.current = owner
	return nil
}

// Release publishes an empty record, ending this host's binding for the
// directory. The chain is append-only, so releasing is a claim of nothing
// rather than a deletion.
func (c *OwnerChain) Release(hostKey, workingDirectory string) error {
	return c.Claim(Owner{WorkingDirectory: workingDirectory, HostKey: hostKey})
}
