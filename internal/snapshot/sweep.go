package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Sweep removes blobs that no session journal references.
//
// A blob is only removed once it has been unreferenced for grace. The delay is
// what makes the sweep safe without locking: another machine may have published
// a blob moments ago and not yet appended the record that names it, and this
// process cannot see that record, nor coordinate with the write. Waiting longer
// than that window costs disk and risks nothing, while deleting early destroys
// content a live session still needs.
//
// The old design instead deleted unreferenced blobs inside the transaction of
// every file-mutating tool call, scanning the largest table in the database on
// each edit, and decided liveness from rows other machines were writing.
func (s *Service) Sweep(grace time.Duration, limit int) (int64, error) {
	if s == nil || s.files.root == "" {
		return 0, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	live, err := s.liveBlobs()
	if err != nil {
		return 0, err
	}

	root := filepath.Join(s.files.root, blobsDir)
	cutoff := time.Now().Add(-grace)
	var removed int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || removed >= int64(limit) {
			return nil
		}
		if _, ok := live[entry.Name()]; ok {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return removed, fmt.Errorf("snapshot: sweep blobs: %w", err)
	}
	return removed, nil
}

// liveBlobs collects every hash referenced by any session's journal, including
// sessions belonging to other machines: they share the blob store, so their
// references are what stop this sweep from deleting content they still need.
func (s *Service) liveBlobs() (map[string]struct{}, error) {
	live := make(map[string]struct{})
	entries, err := os.ReadDir(filepath.Join(s.files.root, journalsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return live, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot: list journals: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		records, err := s.files.records(entry.Name())
		if err != nil {
			// An unreadable journal is treated as referencing everything it
			// might contain, by refusing to sweep at all: deleting a blob some
			// session needs is worse than keeping garbage.
			return nil, err
		}
		for digest := range referenced(records) {
			live[digest] = struct{}{}
		}
	}
	return live, nil
}

// RemoveSession deletes one session's undo history.
func (s *Service) RemoveSession(sessionID string) error {
	if s == nil || s.files.root == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files.removeSession(sessionID)
}
