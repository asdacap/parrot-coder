package process

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/security"
)

type sandbox interface {
	command(shell, script, cwd string, profile security.SecurityProfile, temporaryDirectory string) (string, []string, error)
	temporaryDirectory(string) string
}

type unsupportedSandbox struct{ platform string }

func (s unsupportedSandbox) command(_, _, _ string, _ security.SecurityProfile, _ string) (string, []string, error) {
	return "", nil, errors.New("no sandbox backend is available for " + s.platform)
}

func (unsupportedSandbox) temporaryDirectory(path string) string { return path }

// sandboxProfile is a concrete security.SecurityProfile that combines a base
// profile with session-enriched paths. It is constructed by the Runner before
// being passed to the sandbox backend.
type sandboxProfile struct {
	readOnly   bool
	readPaths  []string
	writePaths []string
	denyWrite  []string
	rules      []security.Rule
}

func (p *sandboxProfile) IsReadOnly() bool          { return p.readOnly }
func (p *sandboxProfile) AllowReadPaths() []string  { return p.readPaths }
func (p *sandboxProfile) AllowWritePaths() []string { return p.writePaths }
func (p *sandboxProfile) DenyWritePaths() []string  { return p.denyWrite }
func (p *sandboxProfile) Rules() []security.Rule    { return p.rules }

func protectedWorkspacePaths(root, cwd string) []string {
	relative, err := filepath.Rel(root, cwd)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return nil
	}
	directories := []string{root}
	if relative != "." {
		directory := root
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			directory = filepath.Join(directory, part)
			directories = append(directories, directory)
		}
	}
	paths := make([]string, 0, len(directories)*3)
	for _, directory := range directories {
		for _, name := range []string{".parrot", "parrot.yaml"} {
			path := filepath.Join(directory, name)
			if _, err := os.Lstat(path); err == nil {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// linkedGitCommonDirectory returns the external common Git directory for a
// linked worktree. The backlink and standard worktrees layout checks avoid
// treating an arbitrary project-controlled .git file as a write capability.
func linkedGitCommonDirectory(root string) (string, bool) {
	dotGit := filepath.Join(root, ".git")
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	gitDirectory := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if !filepath.IsAbs(gitDirectory) {
		gitDirectory = filepath.Join(root, gitDirectory)
	}
	gitDirectory, err = filepath.EvalSymlinks(gitDirectory)
	if err != nil {
		return "", false
	}

	backlink, err := os.ReadFile(filepath.Join(gitDirectory, "gitdir"))
	if err != nil {
		return "", false
	}
	backlinkPath := strings.TrimSpace(string(backlink))
	if !filepath.IsAbs(backlinkPath) {
		backlinkPath = filepath.Join(gitDirectory, backlinkPath)
	}
	backlinkPath, err = filepath.Abs(backlinkPath)
	if err != nil || filepath.Clean(backlinkPath) != dotGit {
		return "", false
	}

	commonData, err := os.ReadFile(filepath.Join(gitDirectory, "commondir"))
	if err != nil {
		return "", false
	}
	commonDirectory := strings.TrimSpace(string(commonData))
	if !filepath.IsAbs(commonDirectory) {
		commonDirectory = filepath.Join(gitDirectory, commonDirectory)
	}
	commonDirectory, err = filepath.EvalSymlinks(commonDirectory)
	if err != nil || filepath.Dir(filepath.Dir(gitDirectory)) != commonDirectory {
		return "", false
	}
	info, err := os.Stat(commonDirectory)
	return commonDirectory, err == nil && info.IsDir()
}
