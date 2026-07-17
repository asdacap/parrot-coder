// Package atomicfile publishes small files so a concurrent reader observes
// either the previous contents or the new contents, never a mixture.
//
// Publication is a rename onto the target. Rename replaces the directory entry
// in one step, so a reader holding the old inode keeps reading consistent data
// while the new inode becomes visible to the next open. This is the only
// property Parrot relies on for shared files: it holds on a local filesystem
// and on NFS, and it does not require a working lock manager.
//
// Rename is atomic publication, not compare-and-swap. Two writers may each read
// a value, each stage a replacement, and each rename; the second silently wins.
// Callers that must detect a competing writer need a version-named target and
// [Link], which reports [fs.ErrExist] instead of overwriting.
package atomicfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxBytes bounds a document read into memory.
const MaxBytes = 16 << 20

// Write publishes data at path by staging a temporary file beside it and
// renaming. The parent directory must already exist.
func Write(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("atomicfile: stage %s: %w", path, err)
	}
	name := temp.Name()
	defer func() {
		temp.Close()
		if err != nil {
			os.Remove(name)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("atomicfile: restrict %s: %w", name, err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", name, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("atomicfile: publish %s: %w", path, err)
	}
	return nil
}

// WriteJSON publishes value at path as indented JSON.
func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("atomicfile: encode %s: %w", path, err)
	}
	return Write(path, append(data, '\n'))
}

// ReadJSON decodes path into target, rejecting unknown fields and trailing
// values so a truncated or foreign document fails loudly.
func ReadJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return fmt.Errorf("atomicfile: read %s: %w", path, err)
	}
	if len(data) > MaxBytes {
		return fmt.Errorf("atomicfile: %s exceeds %d bytes", path, MaxBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("atomicfile: decode %s: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("atomicfile: %s has multiple JSON values", path)
		}
		return fmt.Errorf("atomicfile: decode %s: %w", path, err)
	}
	return nil
}

// Link publishes value at path as JSON only if path does not exist, reporting
// [fs.ErrExist] when another writer created it first.
//
// This is Parrot's only compare-and-swap primitive. It is built on link rather
// than rename because link refuses to replace an existing name, and because the
// server enforces that refusal without consulting a lock manager: it stays
// correct on an NFS mount where every advisory lock is local-only.
func Link(path string, value any) (err error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("atomicfile: encode %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("atomicfile: stage %s: %w", path, err)
	}
	name := temp.Name()
	defer func() {
		temp.Close()
		os.Remove(name)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("atomicfile: restrict %s: %w", name, err)
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", name, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close %s: %w", name, err)
	}
	// Link reports EEXIST rather than replacing, which is what makes the
	// caller's read-decide-write cycle detectable as lost.
	if err := os.Link(name, path); err != nil {
		return err
	}
	return nil
}
