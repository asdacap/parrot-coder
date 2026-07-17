package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// ErrNoSession reports that a session directory does not exist.
var ErrNoSession = errors.New("store: session does not exist")

// Registry resolves a session identifier to that session's database.
//
// Each session owns one file, so a process only ever writes the sessions it is
// running. Handles are cached because a session is opened once per turn but
// used many times within it, and reopening would repay the migration check on
// every call.
type Registry struct {
	state   string
	hostKey string

	mu     sync.Mutex
	open   map[string]*DB
	closed bool
}

func NewRegistry(state, hostKey string) *Registry {
	return &Registry{state: state, hostKey: hostKey, open: make(map[string]*DB)}
}

// State is the directory holding every session directory.
func (r *Registry) State() string { return r.state }

// HostKey identifies this machine.
func (r *Registry) HostKey() string { return r.hostKey }

// Session opens a session's database for writing, reporting [ErrNoSession] if
// the session does not exist and [ErrForeignHost] if another machine owns it.
func (r *Registry) Session(ctx context.Context, sessionID string) (*DB, error) {
	if sessionID == "" {
		return nil, errors.New("store: session ID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("store: registry is closed")
	}
	if db, ok := r.open[sessionID]; ok {
		return db, nil
	}
	if _, err := os.Stat(SessionDir(r.state, sessionID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoSession, sessionID)
		}
		return nil, fmt.Errorf("store: locate session %s: %w", sessionID, err)
	}
	db, err := OpenSessionOwned(ctx, r.state, sessionID, r.hostKey)
	if err != nil {
		return nil, err
	}
	r.open[sessionID] = db
	return db, nil
}

// Create makes a new session directory and its database.
func (r *Registry) Create(ctx context.Context, sessionID string) (*DB, error) {
	if sessionID == "" {
		return nil, errors.New("store: session ID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("store: registry is closed")
	}
	if err := CreateSessionDir(r.state, sessionID); err != nil {
		return nil, fmt.Errorf("store: create session %s: %w", sessionID, err)
	}
	db, err := OpenSession(ctx, DatabasePath(r.state, sessionID))
	if err != nil {
		return nil, err
	}
	r.open[sessionID] = db
	return db, nil
}

// Remove closes and deletes a session.
func (r *Registry) Remove(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if db, ok := r.open[sessionID]; ok {
		db.Close()
		delete(r.open, sessionID)
	}
	if _, err := os.Stat(SessionDir(r.state, sessionID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNoSession, sessionID)
		}
		return err
	}
	return RemoveSession(r.state, sessionID)
}

// Close releases every open session database.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	var errs []error
	for id, db := range r.open {
		if err := db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close session %s: %w", id, err))
		}
		delete(r.open, id)
	}
	return errors.Join(errs...)
}
