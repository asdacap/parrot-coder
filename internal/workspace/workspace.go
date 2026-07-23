// Package workspace resolves untrusted paths for workspace-scoped operations
// and explicit read-only host access.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidPath = errors.New("invalid path")
	ErrOutsideRoot = errors.New("path is outside permitted roots")
)

// ExternalRoot is an explicit capability for workspace-scoped operations on a
// canonical directory outside the workspace. Values can only be constructed by
// NewExternalRoot.
type ExternalRoot struct{ path string }

func NewExternalRoot(path string) (ExternalRoot, error) {
	p, err := canonicalExistingDirectory(path)
	if err != nil {
		return ExternalRoot{}, err
	}
	return ExternalRoot{path: p}, nil
}

func (r ExternalRoot) Path() string { return r.path }

// ExternalPath is an explicit capability for an existing canonical file or
// directory outside the workspace. File capabilities authorize only that exact
// path; directory capabilities authorize descendants as well.
type ExternalPath struct {
	path      string
	directory bool
}

func NewExternalPath(path string) (ExternalPath, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return ExternalPath{}, ErrInvalidPath
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ExternalPath{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return ExternalPath{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ExternalPath{}, err
	}
	return ExternalPath{path: filepath.Clean(resolved), directory: info.IsDir()}, nil
}

func (p ExternalPath) Path() string { return p.path }

// Workspace is immutable after construction and safe for concurrent use.
type Workspace struct {
	root          string
	external      []string
	externalPaths []ExternalPath
}

func New(root string, external ...ExternalRoot) (*Workspace, error) {
	p, err := canonicalExistingDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	ext := make([]string, 0, len(external))
	for _, capability := range external {
		if capability.path == "" {
			return nil, fmt.Errorf("external root: %w", ErrInvalidPath)
		}
		ext = append(ext, capability.path)
	}
	return &Workspace{root: p, external: ext}, nil
}

func (w *Workspace) Root() string { return w.root }

// WithExternalPaths returns a workspace view extended with narrow external
// capabilities. The receiver is not modified.
func (w *Workspace) WithExternalPaths(paths ...ExternalPath) *Workspace {
	if w == nil || len(paths) == 0 {
		return w
	}
	view := &Workspace{
		root:          w.root,
		external:      append([]string(nil), w.external...),
		externalPaths: append([]ExternalPath(nil), w.externalPaths...),
	}
	view.externalPaths = append(view.externalPaths, paths...)
	return view
}

// Contains reports whether path is within the canonical workspace root.
// Explicit external root capabilities are not included.
func (w *Workspace) Contains(path string) bool { return contains(w.root, path) }

// ResolveRead resolves all symlinks and requires the target to exist.
func (w *Workspace) ResolveRead(path string) (string, error) {
	candidate, root, err := w.lexical(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if !contains(root, resolved) {
		return "", ErrOutsideRoot
	}
	return filepath.Clean(resolved), nil
}

// ResolveReadOnly resolves an existing path for a non-mutating operation.
// Relative paths remain confined to the workspace, while an explicit absolute
// path may identify any file readable by the process.
func (w *Workspace) ResolveReadOnly(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return w.ResolveRead(path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", ErrInvalidPath
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// ResolveReadOnlyWithin resolves an existing path and requires the result to
// remain within root. Root must already be canonical.
func (w *Workspace) ResolveReadOnlyWithin(root, path string) (string, error) {
	resolved, err := w.ResolveReadOnly(path)
	if err != nil {
		return "", err
	}
	if !contains(root, resolved) {
		return "", ErrOutsideRoot
	}
	return resolved, nil
}

// ResolveCreate validates the nearest existing parent and returns a canonical
// path suitable for creation. Callers performing mutations must resolve again
// immediately before changing the filesystem.
func (w *Workspace) ResolveCreate(path string) (string, error) {
	candidate, root, err := w.lexical(path)
	if err != nil {
		return "", err
	}
	parent := candidate
	var suffix []string
	for {
		_, statErr := os.Lstat(parent)
		if statErr == nil {
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", statErr
		}
		suffix = append(suffix, filepath.Base(parent))
		parent = next
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	if !contains(root, resolved) {
		return "", ErrOutsideRoot
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
	}
	if !contains(root, resolved) {
		return "", ErrOutsideRoot
	}
	return filepath.Clean(resolved), nil
}

func (w *Workspace) lexical(path string) (candidate, root string, err error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", "", ErrInvalidPath
	}
	if filepath.IsAbs(path) {
		candidate = filepath.Clean(path)
		for _, allowed := range append([]string{w.root}, w.external...) {
			if contains(allowed, candidate) {
				return candidate, allowed, nil
			}
		}
		for _, allowed := range w.externalPaths {
			if candidate == allowed.path || allowed.directory && contains(allowed.path, candidate) {
				return candidate, allowed.path, nil
			}
		}
		return "", "", ErrOutsideRoot
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", ErrOutsideRoot
	}
	candidate = filepath.Join(w.root, clean)
	if !contains(w.root, candidate) {
		return "", "", ErrOutsideRoot
	}
	return candidate, w.root, nil
}

func contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func canonicalExistingDirectory(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", ErrInvalidPath
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return filepath.Clean(resolved), nil
}
