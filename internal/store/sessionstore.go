package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/atomicfile"
)

// Layout below the state directory.
const (
	sessionsDir = "sessions"
	databaseKey = "session.db"
	metaKey     = "meta.json"
)

// MetaVersion is the schema version of a published session index entry.
const MetaVersion = 1

// ErrForeignHost reports that a session is bound to a different machine.
var ErrForeignHost = errors.New("store: session belongs to another host")

// Meta is the published index entry for one session.
//
// It exists so that listing sessions never opens a database another host may be
// writing. A reader with no working cross-host lock cannot open such a database
// safely, but it can always read a file that is replaced by rename. Meta is a
// projection: the session database remains the source of truth, and Meta can be
// rebuilt from it.
type Meta struct {
	Version         int    `json:"version"`
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	ProjectID       string `json:"project_id"`
	ProjectRoot     string `json:"project_root"`
	Title           string `json:"title"`
	Agent           string `json:"agent"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Variant         string `json:"variant"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	HostKey         string `json:"host_key"`
	PID             int    `json:"pid"`
}

// SessionsRoot is the directory holding every session directory.
func SessionsRoot(state string) string { return filepath.Join(state, sessionsDir) }

// SessionDir is one session's directory.
func SessionDir(state, sessionID string) string {
	return filepath.Join(SessionsRoot(state), sessionID)
}

// DatabasePath is one session's database file.
func DatabasePath(state, sessionID string) string {
	return filepath.Join(SessionDir(state, sessionID), databaseKey)
}

// MetaPath is one session's published index entry.
func MetaPath(state, sessionID string) string {
	return filepath.Join(SessionDir(state, sessionID), metaKey)
}

// CreateSessionDir creates a session directory, reporting [fs.ErrExist] if the
// session already exists.
func CreateSessionDir(state, sessionID string) error {
	if err := os.MkdirAll(SessionsRoot(state), 0o700); err != nil {
		return fmt.Errorf("store: create sessions directory: %w", err)
	}
	if err := os.Mkdir(SessionDir(state, sessionID), 0o700); err != nil {
		return err
	}
	return nil
}

// WriteMeta publishes a session index entry.
func WriteMeta(state string, meta Meta) error {
	meta.Version = MetaVersion
	return atomicfile.WriteJSON(MetaPath(state, meta.ID), meta)
}

// ReadMeta reads one published session index entry.
func ReadMeta(state, sessionID string) (Meta, error) {
	var meta Meta
	if err := atomicfile.ReadJSON(MetaPath(state, sessionID), &meta); err != nil {
		return Meta{}, err
	}
	if meta.Version != MetaVersion {
		return Meta{}, fmt.Errorf("store: session %s index version %d is unsupported", sessionID, meta.Version)
	}
	if meta.Name == "" {
		meta.Name = legacySessionName(meta.Title, meta.Agent)
	}
	return meta, nil
}

func legacySessionName(title, agent string) string {
	prefix, suffix := "Subtask ", " ["+agent+"]"
	if agent == "" || !strings.HasPrefix(title, prefix) || !strings.HasSuffix(title, suffix) {
		return ""
	}
	name := strings.TrimSuffix(strings.TrimPrefix(title, prefix), suffix)
	if name == "" {
		return ""
	}
	return name
}

// ListMeta reads every published session index entry, newest first.
//
// An entry that cannot be read is skipped rather than failing the listing: it
// may be a session being created right now, or one published by a newer binary
// on another host. Listing every other session is more useful than reporting
// nothing. The skipped identifiers are returned so a caller can warn.
func ListMeta(state string) (metas []Meta, skipped []string, err error) {
	entries, err := os.ReadDir(SessionsRoot(state))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list sessions: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := ReadMeta(state, entry.Name())
		if err != nil {
			skipped = append(skipped, entry.Name())
			continue
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].CreatedAt != metas[j].CreatedAt {
			return metas[i].CreatedAt > metas[j].CreatedAt
		}
		return metas[i].ID > metas[j].ID
	})
	return metas, skipped, nil
}

// RemoveSession deletes a session directory and everything in it.
func RemoveSession(state, sessionID string) error {
	return os.RemoveAll(SessionDir(state, sessionID))
}

// OpenSessionOwned opens a session's database for writing after checking the
// published index does not bind the session to a different machine.
//
// The check is advisory. Nothing in the filesystem prevents two hosts from
// opening one database, and this mount grants advisory locks locally without
// telling any other host, so no lock could prevent it either. The stamp turns
// the likely accident -- resuming on a second machine a session the first is
// still running -- into a refusal instead of silent corruption. It is not a
// mutual exclusion mechanism and must not be described as one.
func OpenSessionOwned(ctx context.Context, state, sessionID, hostKey string) (*DB, error) {
	meta, err := ReadMeta(state, sessionID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A session directory without an index is being created or was
		// interrupted; the database itself decides whether it exists.
	case err != nil:
		return nil, err
	case meta.HostKey != "" && meta.HostKey != hostKey:
		return nil, fmt.Errorf("%w: %s is bound to %s", ErrForeignHost, sessionID, meta.HostKey)
	}
	return OpenSession(ctx, DatabasePath(state, sessionID))
}

// StampOwner records this machine as the session's owner in the published index.
func StampOwner(state, sessionID, hostKey string, pid int) error {
	meta, err := ReadMeta(state, sessionID)
	if err != nil {
		return err
	}
	meta.HostKey = hostKey
	meta.PID = pid
	meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return WriteMeta(state, meta)
}
