//go:build darwin

package process

import (
	"os"

	"golang.org/x/sys/unix"
)

func clearTemporaryDirectoryFlags(path string, entry os.DirEntry) error {
	if entry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	return unix.Chflags(path, 0)
}
