package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	sessionStateDirectoryName = "session"
	scratchDirectoryName      = "scratch"
)

// SessionStateDirectory is the private application state owned by one agent
// session. Scratch is the only part exposed as a writable agent capability.
type SessionStateDirectory struct {
	path string
}

func (d SessionStateDirectory) Path() string { return d.path }

func (d SessionStateDirectory) ScratchPath() string {
	if d.path == "" {
		return ""
	}
	return filepath.Join(d.path, scratchDirectoryName)
}

// SessionStateDirectories resolves and provisions per-session state.
type SessionStateDirectories interface {
	Directory(sessionID string) (SessionStateDirectory, error)
	Prepare(sessionID string) (SessionStateDirectory, error)
	Remove(sessionID string) error
}

type localSessionStateDirectories struct {
	root string
}

// NewSessionStateDirectories creates a filesystem-backed per-session state
// provider rooted below the application state directory.
func NewSessionStateDirectories(stateRoot string) (SessionStateDirectories, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) {
		return nil, errors.New("agent: application state directory must be absolute")
	}
	return localSessionStateDirectories{root: filepath.Clean(stateRoot)}, nil
}

func (d localSessionStateDirectories) Directory(sessionID string) (SessionStateDirectory, error) {
	if !validSessionStateID(sessionID) {
		return SessionStateDirectory{}, fmt.Errorf("agent: invalid session ID for state directory %q", sessionID)
	}
	return SessionStateDirectory{path: filepath.Join(d.root, sessionStateDirectoryName, sessionID)}, nil
}

func (d localSessionStateDirectories) Prepare(sessionID string) (SessionStateDirectory, error) {
	directory, err := d.Directory(sessionID)
	if err != nil {
		return SessionStateDirectory{}, err
	}
	if err := os.MkdirAll(directory.ScratchPath(), 0o700); err != nil {
		return SessionStateDirectory{}, fmt.Errorf("agent: create session scratch directory: %w", err)
	}
	for _, path := range []string{directory.Path(), directory.ScratchPath()} {
		if err := os.Chmod(path, 0o700); err != nil {
			return SessionStateDirectory{}, fmt.Errorf("agent: secure session state directory: %w", err)
		}
	}
	return directory, nil
}

func (d localSessionStateDirectories) Remove(sessionID string) error {
	directory, err := d.Directory(sessionID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory.Path()); err != nil {
		return fmt.Errorf("agent: remove session state directory: %w", err)
	}
	return nil
}

func validSessionStateID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." &&
		!filepath.IsAbs(sessionID) && !strings.ContainsAny(sessionID, `/\\`)
}
