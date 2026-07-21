//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type linuxSandbox struct {
	workspace string
}

func platformSandbox(ws *workspace.Workspace, _ string) sandbox {
	return linuxSandbox{workspace: ws.Root()}
}

func (s linuxSandbox) command(shell, script, cwd string, profile security.SecurityProfile, temporaryDirectory string) (string, []string, error) {
	bwrap, err := executableOutsideWorkspace("bwrap", s.workspace)
	if err != nil {
		return "", nil, errors.New("bubblewrap is required; install bwrap and ensure unprivileged user namespaces are enabled")
	}
	if cwd == "/tmp" {
		return "", nil, errors.New("the sandbox's private /tmp cannot be used as an external working directory")
	}
	externalTmpCwd := within(cwd, "/tmp") && !within(cwd, s.workspace)

	// Create an empty file for a writable /dev/null bind mount.
	// bwrap's --dev mounts device nodes from the host, but when the host's
	// root is read-only (e.g. a container) those bind mounts are also
	// read-only.  We replace /dev/null with a writable regular file
	// so that tools like git, go, and shells can always open it for writing.
	nullPath := filepath.Join(temporaryDirectory, ".parrot-null")
	if err := os.WriteFile(nullPath, nil, 0666); err != nil {
		return "", nil, fmt.Errorf("create dev-null file: %w", err)
	}

	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-user",
		"--unshare-pid",
		"--cap-drop", "ALL",
	}
	// Readable paths are mounted first because they establish the base
	// filesystem. A profile's read path is typically "/", and binding it after
	// the synthetic mounts below would remount the host over them, replacing
	// the private /tmp and the writable /dev/null with their host counterparts.
	for _, path := range profile.AllowReadPaths() {
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args,
		"--tmpfs", "/dev",
		"--bind", nullPath, "/dev/null",
		"--dev-bind", "/dev/zero", "/dev/zero",
		"--dev-bind", "/dev/random", "/dev/random",
		"--dev-bind", "/dev/urandom", "/dev/urandom",
		"--dev-bind", "/dev/full", "/dev/full",
		"--dev-bind", "/dev/tty", "/dev/tty",
		"--symlink", "/proc/self/fd", "/dev/fd",
		"--symlink", "/proc/self/fd/0", "/dev/stdin",
		"--symlink", "/proc/self/fd/1", "/dev/stdout",
		"--symlink", "/proc/self/fd/2", "/dev/stderr",
		"--dir", "/dev/shm",
		"--dir", "/dev/pts",
		"--proc", "/proc",
		"--bind", temporaryDirectory, "/tmp",
		"--setenv", "TMPDIR", "/tmp",
	)
	if externalTmpCwd {
		args = append(args, "--dir", cwd)
	}
	if maskedBySandbox(cwd) {
		args = append(args, "--ro-bind", cwd, cwd)
	}
	for _, path := range profile.AllowWritePaths() {
		args = append(args, "--bind", path, path)
	}
	for _, path := range profile.DenyWritePaths() {
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args, "--chdir", cwd, "--", shell, "-c", script)
	return bwrap, args, nil
}

func (linuxSandbox) temporaryDirectory(string) string { return "/tmp" }

func maskedBySandbox(path string) bool {
	for _, root := range []string{"/tmp", "/dev", "/proc"} {
		if path != root && within(path, root) {
			return true
		}
	}
	return false
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
