// Package project resolves project identities from the current working directory.
package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
)

// Info describes the project containing a working directory.
type Info struct {
	ID   string
	Root string
}

// Resolve finds the project containing cwd. The project identity is always
// derived from the canonical path of cwd, so every directory is its own project.
func Resolve(ctx context.Context, cwd string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return Info{}, fmt.Errorf("resolve working directory: %w", err)
	}
	root := canonicalPath(absolute)
	return Info{
		ID:   StableID(root),
		Root: root,
	}, nil
}

func canonicalPath(path string) string {
	path, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		path = evaluated
	}
	return filepath.Clean(path)
}

// StableID returns a stable identifier derived from the canonical root path.
func StableID(root string) string {
	digest := sha256.Sum256([]byte("path\x00" + canonicalPath(root)))
	return hex.EncodeToString(digest[:])
}
