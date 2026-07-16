//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type linuxSandbox struct {
	workspace        string
	workingDirectory string
}

func platformSandbox(ws *workspace.Workspace, workingDirectory string) sandbox {
	return linuxSandbox{workspace: ws.Root(), workingDirectory: workingDirectory}
}

func (s linuxSandbox) command(shell, script, cwd string) (string, []string, error) {
	bwrap, err := executableOutsideWorkspace("bwrap", s.workspace)
	if err != nil {
		return "", nil, errors.New("bubblewrap is required; install bwrap and ensure unprivileged user namespaces are enabled")
	}
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-user",
		"--unshare-pid",
		"--cap-drop", "ALL",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--setenv", "TMPDIR", "/tmp",
		"--bind", s.workspace, s.workspace,
	}
	if commonDirectory, ok := linkedGitCommonDirectory(s.workspace); ok {
		args = append(args, "--bind", commonDirectory, commonDirectory)
	}
	for _, path := range protectedWorkspacePaths(s.workspace, s.workingDirectory) {
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args, "--chdir", cwd, "--", shell, "-c", script)
	return bwrap, args, nil
}

func executableOutsideWorkspace(name, root string) (string, error) {
	path, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		candidate, err := filepath.Abs(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		candidate, err = filepath.EvalSymlinks(candidate)
		if err != nil || within(candidate, root) {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found outside workspace", name)
}

func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
