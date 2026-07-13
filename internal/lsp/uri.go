package lsp

import (
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// FileURI returns the canonical file URI for an existing path in workspace.
func FileURI(workspace, path string) (DocumentURI, error) {
	root, resolved, err := containedPath(workspace, path)
	if err != nil {
		return "", err
	}
	if !containsPath(root, resolved) {
		return "", errors.New("lsp: path is outside workspace")
	}
	slash := filepath.ToSlash(resolved)
	if runtime.GOOS == "windows" && !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}
	return DocumentURI((&url.URL{Scheme: "file", Path: slash}).String()), nil
}

// PathFromURI converts a file URI to a canonical workspace-contained path.
func PathFromURI(workspace string, uri DocumentURI) (string, error) {
	u, err := url.Parse(string(uri))
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("lsp: invalid file URI")
	}
	path := filepath.FromSlash(u.Path)
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == filepath.Separator && path[2] == ':' {
		path = path[1:]
	}
	_, resolved, err := containedPath(workspace, path)
	return resolved, err
}

func containedPath(workspace, path string) (string, string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if !containsPath(root, path) {
		return "", "", errors.New("lsp: path is outside workspace")
	}
	return filepath.Clean(root), filepath.Clean(path), nil
}

func containsPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
