package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	sessionStateDirectoryName     = "session"
	userSessionStateDirectoryName = "user-session"
	scratchDirectoryName          = "scratch"
	queuesDirectoryName           = "queues"
)

// UserSessionStateDirectory is the private application state owned by one agent
// session. Scratch is exposed as a writable agent capability.
type UserSessionStateDirectory struct {
	path string
}

func (d UserSessionStateDirectory) Path() string { return d.path }

func (d UserSessionStateDirectory) ScratchPath() string {
	if d.path == "" {
		return ""
	}
	return filepath.Join(d.path, scratchDirectoryName)
}

// UserSessionQueuesPath resolves the durable queue directory owned by one
// logical user session. Project IDs are the stable user-session identity in the
// single-project application composition.
func UserSessionQueuesPath(stateRoot, projectID string) (string, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) {
		return "", errors.New("agent: application state directory must be absolute")
	}
	if !validSessionStateID(projectID) {
		return "", fmt.Errorf("agent: invalid project ID for user session state %q", projectID)
	}
	return filepath.Join(filepath.Clean(stateRoot), userSessionStateDirectoryName, projectID, queuesDirectoryName), nil
}

// UserSessionStateDirectories resolves and provisions per-agent-session state.
type UserSessionStateDirectories interface {
	Directory(sessionID string) (UserSessionStateDirectory, error)
	Prepare(sessionID string) (UserSessionStateDirectory, error)
	Remove(sessionID string) error
}

type localUserSessionStateDirectories struct {
	root string
}

// NewUserSessionStateDirectories creates a filesystem-backed agent-session
// state provider rooted below the application state directory.
func NewUserSessionStateDirectories(stateRoot string) (UserSessionStateDirectories, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) {
		return nil, errors.New("agent: application state directory must be absolute")
	}
	return localUserSessionStateDirectories{root: filepath.Clean(stateRoot)}, nil
}

func (d localUserSessionStateDirectories) Directory(sessionID string) (UserSessionStateDirectory, error) {
	if !validSessionStateID(sessionID) {
		return UserSessionStateDirectory{}, fmt.Errorf("agent: invalid session ID for state directory %q", sessionID)
	}
	return UserSessionStateDirectory{path: filepath.Join(d.root, sessionStateDirectoryName, sessionID)}, nil
}

func (d localUserSessionStateDirectories) Prepare(sessionID string) (UserSessionStateDirectory, error) {
	directory, err := d.Directory(sessionID)
	if err != nil {
		return UserSessionStateDirectory{}, err
	}
	if err := os.MkdirAll(directory.ScratchPath(), 0o700); err != nil {
		return UserSessionStateDirectory{}, fmt.Errorf("agent: create session state subdirectory: %w", err)
	}
	for _, path := range []string{directory.Path(), directory.ScratchPath()} {
		if err := os.Chmod(path, 0o700); err != nil {
			return UserSessionStateDirectory{}, fmt.Errorf("agent: secure session state directory: %w", err)
		}
	}
	return directory, nil
}

func (d localUserSessionStateDirectories) Remove(sessionID string) error {
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
