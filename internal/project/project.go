// Package project resolves Git metadata and stable project identities.
package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Info describes the repository containing a working directory. WorktreeDir
// is Git's per-worktree metadata directory; CommonDir is shared by linked
// worktrees; Root is the checked-out filesystem root.
type Info struct {
	ID          string
	Root        string
	WorktreeDir string
	CommonDir   string
	Remote      string
	RootCommit  string
}

// Resolve finds the project containing cwd. Outside Git, cwd itself is the
// project root and its canonical path supplies the stable identity fallback.
func Resolve(ctx context.Context, cwd string) (Info, error) {
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return Info{}, fmt.Errorf("resolve working directory: %w", err)
	}
	absolute = filepath.Clean(absolute)

	output, err := git(ctx, absolute, "rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		if ctx.Err() != nil {
			return Info{}, ctx.Err()
		}
		var executableError *exec.Error
		if errors.As(err, &executableError) {
			return Info{}, fmt.Errorf("run git: %w", err)
		}
		root := canonicalPath(absolute)
		return Info{ID: StableID("", "", root), Root: root}, nil
	}
	lines := nonemptyLines(output)
	if len(lines) != 3 {
		return Info{}, fmt.Errorf("git rev-parse returned %d lines, want 3", len(lines))
	}
	root := canonicalPath(lines[0])
	worktreeDir := absoluteGitPath(absolute, lines[1])
	commonDir := absoluteGitPath(absolute, lines[2])

	remote := ""
	if output, remoteErr := git(ctx, root, "remote", "get-url", "origin"); remoteErr == nil {
		remote, err = NormalizeRemoteURL(strings.TrimSpace(output))
		if err != nil {
			return Info{}, fmt.Errorf("normalize origin remote: %w", err)
		}
	}
	rootCommit := ""
	if output, commitErr := git(ctx, root, "rev-list", "--max-parents=0", "HEAD"); commitErr == nil {
		commits := nonemptyLines(output)
		sort.Strings(commits)
		if len(commits) > 0 {
			rootCommit = commits[0]
		}
	}

	return Info{
		ID:          StableID(remote, rootCommit, root),
		Root:        root,
		WorktreeDir: worktreeDir,
		CommonDir:   commonDir,
		Remote:      remote,
		RootCommit:  rootCommit,
	}, nil
}

func git(ctx context.Context, cwd string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = cwd
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func nonemptyLines(output string) []string {
	var result []string
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func absoluteGitPath(cwd, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return canonicalPath(path)
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

// NormalizeRemoteURL maps common Git remote forms to a credential-free
// host/path identity. URL schemes and a trailing .git do not affect identity.
func NormalizeRemoteURL(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", nil
	}

	// Git's SCP-like syntax is not understood by net/url.
	if !strings.Contains(remote, "://") {
		colon := strings.IndexByte(remote, ':')
		if colon > 0 && !filepath.IsAbs(remote) {
			host := remote[:colon]
			if at := strings.LastIndexByte(host, '@'); at >= 0 {
				host = host[at+1:]
			}
			return normalizeHostPath(host, remote[colon+1:])
		}
		return "file://" + filepath.ToSlash(canonicalPath(remote)), nil
	}

	parsed, err := url.Parse(remote)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "file" {
		return "", nil
	}
	if parsed.Hostname() == "" || parsed.Path == "" {
		return "", fmt.Errorf("remote %q must contain host and path", remote)
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port != "" && !((parsed.Scheme == "ssh" && port == "22") || (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80")) {
		host += ":" + port
	}
	return normalizeHostPath(host, parsed.Path)
}

func normalizeHostPath(host, path string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" {
		return "", errors.New("remote must contain host and repository path")
	}
	return host + "/" + path, nil
}

// StableID hashes the strongest available project identity. A normalized
// remote is stable across clones, then the root commit, then the canonical path.
func StableID(remote, rootCommit, root string) string {
	kind, identity := "path", canonicalPath(root)
	if rootCommit = strings.TrimSpace(rootCommit); rootCommit != "" {
		kind, identity = "root-commit", strings.ToLower(rootCommit)
	}
	if remote = strings.TrimSpace(remote); remote != "" {
		kind, identity = "remote", remote
	}
	digest := sha256.Sum256([]byte(kind + "\x00" + identity))
	return hex.EncodeToString(digest[:])
}
