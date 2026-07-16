//go:build darwin

package process

import (
	"errors"
	"fmt"
	"os"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

const seatbeltExecutable = "/usr/bin/sandbox-exec"

type darwinSandbox struct {
	workspace        string
	workingDirectory string
}

func platformSandbox(ws *workspace.Workspace, workingDirectory string) sandbox {
	return darwinSandbox{workspace: ws.Root(), workingDirectory: workingDirectory}
}

func (s darwinSandbox) command(shell, script, cwd string, writablePaths []string) (string, []string, error) {
	info, err := os.Stat(seatbeltExecutable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", nil, errors.New("macOS Seatbelt is required but /usr/bin/sandbox-exec is unavailable")
	}
	profile := `(version 1)
(deny default)
(allow process-exec process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
(allow file-read* file-ioctl)
(allow file-write* (subpath (param "WORKSPACE")))
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/tmp"))
(allow file-write-data (literal "/dev/null"))
(allow network*)
(allow sysctl-read)
(allow mach-lookup)
(allow ipc-posix*)
(allow iokit-open)
(allow pseudo-tty)
`
	protected := protectedWorkspacePaths(s.workspace, s.workingDirectory)
	for i := range writablePaths {
		name := fmt.Sprintf("WRITABLE_%d", i)
		profile += fmt.Sprintf("(allow file-write* (literal (param %q)) (subpath (param %q)))\n", name, name)
	}
	for i := range protected {
		name := fmt.Sprintf("PROTECTED_%d", i)
		profile += fmt.Sprintf("(deny file-write* (subpath (param %q)))\n", name)
	}
	args := []string{"-p", profile, "-D", "WORKSPACE=" + s.workspace}
	for i, path := range writablePaths {
		args = append(args, "-D", fmt.Sprintf("WRITABLE_%d=%s", i, path))
	}
	for i, path := range protected {
		args = append(args, "-D", fmt.Sprintf("PROTECTED_%d=%s", i, path))
	}
	args = append(args, shell, "-c", script)
	return seatbeltExecutable, args, nil
}
