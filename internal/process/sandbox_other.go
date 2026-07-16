//go:build !linux && !darwin

package process

import (
	"runtime"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

func platformSandbox(_ *workspace.Workspace, _ string) sandbox {
	return unsupportedSandbox{platform: runtime.GOOS}
}
