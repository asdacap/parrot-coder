package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemoteURL(t *testing.T) {
	tests := map[string]string{
		"git@GitHub.com:Owner/repo.git":                 "github.com/Owner/repo",
		"ssh://git@github.com/Owner/repo.git":           "github.com/Owner/repo",
		"ssh://git@github.com:22/Owner/repo.git":        "github.com/Owner/repo",
		"https://user:secret@GitHub.com/Owner/repo.git": "github.com/Owner/repo",
		"https://github.com:443/Owner/repo.git?q=one":   "github.com/Owner/repo",
	}
	for input, want := range tests {
		got, err := NormalizeRemoteURL(input)
		if err != nil {
			t.Errorf("NormalizeRemoteURL(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeRemoteURLIgnoresFileRemote(t *testing.T) {
	t.Parallel()

	got, err := NormalizeRemoteURL("file:///tmp/repository.git")
	if err != nil {
		t.Fatalf("NormalizeRemoteURL() error = %v", err)
	}
	if got != "" {
		t.Fatalf("NormalizeRemoteURL() = %q, want empty", got)
	}
}

func TestNormalizeRemoteURLRejectsIncompleteURL(t *testing.T) {
	if _, err := NormalizeRemoteURL("https://github.com"); err == nil {
		t.Fatal("accepted remote without repository path")
	}
}

func TestStableIDFallbackOrder(t *testing.T) {
	root := t.TempDir()
	remoteID := StableID("github.com/owner/repo", "abc", root)
	if remoteID != StableID("github.com/owner/repo", "different", filepath.Join(root, "other")) {
		t.Fatal("remote identity did not take precedence")
	}
	commitID := StableID("", "abc", root)
	if commitID != StableID("", "abc", filepath.Join(root, "other")) {
		t.Fatal("root commit identity did not take precedence over path")
	}
	if commitID == StableID("", "def", root) || remoteID == commitID {
		t.Fatal("different identity sources collided")
	}
	if len(remoteID) != 64 {
		t.Fatalf("ID length = %d", len(remoteID))
	}
}

func TestResolveRepositoryAndLinkedWorktree(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repo; touch INJECTED")
	runGit(t, "", "init", repository)
	runGit(t, repository, "config", "user.name", "Test")
	runGit(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "README"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "README")
	runGit(t, repository, "commit", "-m", "initial")
	runGit(t, repository, "remote", "add", "origin", "git@GitHub.com:Owner/repo.git")

	info, err := Resolve(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepository := canonicalPath(repository)
	if info.Root != canonicalRepository || info.WorktreeDir != filepath.Join(canonicalRepository, ".git") || info.CommonDir != filepath.Join(canonicalRepository, ".git") {
		t.Fatalf("Info = %#v", info)
	}
	if info.Remote != "github.com/Owner/repo" || len(info.RootCommit) != 40 || len(info.ID) != 64 {
		t.Fatalf("Info = %#v", info)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(repository), "INJECTED")); !os.IsNotExist(err) {
		t.Fatal("working directory was interpreted by a shell")
	}

	worktree := filepath.Join(filepath.Dir(repository), "linked")
	runGit(t, repository, "worktree", "add", "-b", "linked-test", worktree)
	linked, err := Resolve(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Root != canonicalPath(worktree) || linked.CommonDir != filepath.Join(canonicalRepository, ".git") || linked.WorktreeDir == linked.CommonDir {
		t.Fatalf("linked Info = %#v", linked)
	}
	if linked.ID != info.ID {
		t.Fatalf("linked ID = %q, repository ID = %q", linked.ID, info.ID)
	}
}

func TestResolveOutsideGitUsesPath(t *testing.T) {
	dir := t.TempDir()
	info, err := Resolve(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Root != canonicalPath(dir) || info.WorktreeDir != "" || info.CommonDir != "" || info.ID != StableID("", "", dir) {
		t.Fatalf("Info = %#v", info)
	}
}

func runGit(t *testing.T, dir string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	if dir != "" {
		command.Dir = dir
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}
