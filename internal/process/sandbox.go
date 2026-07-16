package process

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type sandbox interface {
	command(shell, script, cwd string, writablePaths []string) (string, []string, error)
}

type unsupportedSandbox struct{ platform string }

func (s unsupportedSandbox) command(_, _, _ string, _ []string) (string, []string, error) {
	return "", nil, errors.New("no sandbox backend is available for " + s.platform)
}

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
		for _, name := range []string{".git", ".parrot", "parrot.jsonc"} {
			path := filepath.Join(directory, name)
			if _, err := os.Lstat(path); err == nil {
				paths = append(paths, path)
			}
		}
	}
	return paths
}
