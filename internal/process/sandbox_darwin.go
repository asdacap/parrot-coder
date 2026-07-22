//go:build darwin

package process

import (
	"errors"
	"fmt"
	"os"

	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

const seatbeltExecutable = "/usr/bin/sandbox-exec"

type darwinSandbox struct {
	workspace string
}

func platformSandbox(ws *workspace.Workspace, _ string) sandbox {
	return darwinSandbox{workspace: ws.Root()}
}

func (s darwinSandbox) command(shell, script, cwd string, profile security.SecurityProfile, temporaryDirectory string) (string, []string, error) {
	info, err := os.Stat(seatbeltExecutable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", nil, errors.New("macOS Seatbelt is required but /usr/bin/sandbox-exec is unavailable")
	}
	sbProfile := `(version 1)
(deny default)
(allow process-exec process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
(allow file-read* file-ioctl)
(allow file-write-data (literal "/dev/null"))
(allow network*)
(allow sysctl-read)
(allow mach-lookup)
(allow ipc-posix*)
(allow iokit-open)
(allow pseudo-tty)
`
	for i, path := range profile.AllowWritePaths() {
		name := fmt.Sprintf("WRITABLE_%d", i)
		sbProfile += fmt.Sprintf("(allow file-write* (literal (param %q)) (subpath (param %q)))\n", name, name)
	}
	sbProfile += fmt.Sprintf("(allow file-write* (subpath (param %q)))\n", "TEMPORARY_DIRECTORY")
	for i, path := range profile.DenyWritePaths() {
		name := fmt.Sprintf("PROTECTED_%d", i)
		sbProfile += fmt.Sprintf("(deny file-write* (subpath (param %q)))\n", name)
	}
	for i, rule := range profile.Rules() {
		name := fmt.Sprintf("RULE_%d", i)
		switch rule.Action {
		case security.ActionAllowRead:
			sbProfile += fmt.Sprintf("(allow file-read* (subpath (param %q)))\n", name)
		case security.ActionAllowWrite:
			sbProfile += fmt.Sprintf("(allow file-write* (subpath (param %q)))\n", name)
		case security.ActionDenyRead:
			sbProfile += fmt.Sprintf("(deny file-read* (subpath (param %q)))\n", name)
		case security.ActionDenyWrite:
			sbProfile += fmt.Sprintf("(deny file-write* (subpath (param %q)))\n", name)
		}
	}
	args := []string{"-p", sbProfile, "-D", "TEMPORARY_DIRECTORY=" + temporaryDirectory}
	for i, path := range profile.AllowWritePaths() {
		args = append(args, "-D", fmt.Sprintf("WRITABLE_%d=%s", i, path))
	}
	for i, path := range profile.DenyWritePaths() {
		args = append(args, "-D", fmt.Sprintf("PROTECTED_%d=%s", i, path))
	}
	for i, rule := range profile.Rules() {
		args = append(args, "-D", fmt.Sprintf("RULE_%d=%s", i, rule.Path))
	}
	args = append(args, shell, "-c", script)
	return seatbeltExecutable, args, nil
}

func (darwinSandbox) temporaryDirectory(path string) string { return path }
