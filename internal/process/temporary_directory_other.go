//go:build !darwin

package process

import "os"

func clearTemporaryDirectoryFlags(string, os.DirEntry) error { return nil }
