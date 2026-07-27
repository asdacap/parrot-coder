package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/atomicfile"
)

const sessionNamesDir = "session-names"

var ErrSessionNameReserved = errors.New("store: session name is reserved")

type sessionNameReservation struct {
	SessionID string `json:"session_id"`
	HostKey   string `json:"host_key"`
	PID       int    `json:"pid"`
}

type sessionNameChain struct {
	dir     string
	version int
	current sessionNameReservation
}

func sessionNamesRoot(state string) string { return filepath.Join(state, sessionNamesDir) }

func sessionNameDir(state, name string) string {
	return filepath.Join(sessionNamesRoot(state), name)
}

func loadSessionNameChain(state, name string) (*sessionNameChain, error) {
	chain := &sessionNameChain{dir: sessionNameDir(state, name)}
	entries, err := os.ReadDir(chain.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return chain, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read session name chain: %w", err)
	}
	versions := make([]int, 0, len(entries))
	for _, entry := range entries {
		if version, ok := sessionNameVersion(entry.Name()); ok {
			versions = append(versions, version)
		}
	}
	sort.Ints(versions)
	if len(versions) > 0 {
		chain.version = versions[len(versions)-1]
	}
	for index := len(versions) - 1; index >= 0; index-- {
		var reservation sessionNameReservation
		if err := atomicfile.ReadJSON(filepath.Join(chain.dir, sessionNameFile(versions[index])), &reservation); err == nil {
			chain.current = reservation
			return chain, nil
		}
	}
	return chain, nil
}

func sessionNameFile(version int) string { return "v" + strconv.Itoa(version) + ".json" }

func sessionNameVersion(name string) (int, bool) {
	if !strings.HasPrefix(name, "v") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "v"), ".json"))
	return version, err == nil && version > 0
}

func (c *sessionNameChain) claim(reservation sessionNameReservation) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("store: create session name chain: %w", err)
	}
	err := atomicfile.Link(filepath.Join(c.dir, sessionNameFile(c.version+1)), reservation)
	if errors.Is(err, fs.ErrExist) {
		return ErrSessionNameReserved
	}
	if err != nil {
		return fmt.Errorf("store: publish session name reservation: %w", err)
	}
	return nil
}

// ReserveSessionName atomically associates name with sessionID across every
// process sharing state. The reservation remains with the session until it is
// deleted, so published metadata and in-flight creation use the same namespace.
// A dead same-host creator's unpublished reservation is reclaimed; another
// host's liveness is unknowable and its reservation is left intact.
func ReserveSessionName(state, name, sessionID, hostKey string, pid int, alive func(int) bool) error {
	if name == "" || sessionID == "" || hostKey == "" || pid <= 0 || filepath.Base(name) != name {
		return errors.New("store: session name, session ID, host key, and PID are required")
	}
	for {
		chain, err := loadSessionNameChain(state, name)
		if err != nil {
			return err
		}
		current := chain.current
		if current.SessionID != "" {
			_, metaErr := os.Stat(MetaPath(state, current.SessionID))
			published := metaErr == nil
			if metaErr != nil && !errors.Is(metaErr, fs.ErrNotExist) {
				return fmt.Errorf("store: inspect reserved session: %w", metaErr)
			}
			if published || current.HostKey != hostKey || alive == nil || alive(current.PID) {
				return ErrSessionNameReserved
			}
		}
		if err := chain.claim(sessionNameReservation{SessionID: sessionID, HostKey: hostKey, PID: pid}); errors.Is(err, ErrSessionNameReserved) {
			continue
		} else {
			return err
		}
	}
}

// ReleaseSessionName publishes an empty next version only when the current
// reservation still belongs to sessionID. Concurrent release or reuse is retried
// against the newly published version, so cleanup cannot release a later owner.
func ReleaseSessionName(state, name, sessionID string) error {
	if name == "" {
		return nil
	}
	for {
		chain, err := loadSessionNameChain(state, name)
		if err != nil {
			return err
		}
		if chain.current.SessionID != sessionID {
			return nil
		}
		if err := chain.claim(sessionNameReservation{}); errors.Is(err, ErrSessionNameReserved) {
			continue
		} else {
			return err
		}
	}
}

// ReleaseSessionNames releases every current reservation owned by sessionID.
// It supports deletion even when the session's rebuildable metadata is missing
// or unreadable, and is safe to retry after the session directory is gone.
func ReleaseSessionNames(state, sessionID string) error {
	entries, err := os.ReadDir(sessionNamesRoot(state))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: list session name chains: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ReleaseSessionName(state, entry.Name(), sessionID); err != nil {
			return err
		}
	}
	return nil
}
