package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/process"
)

type OutputConfig struct {
	Directory    string
	PreviewBytes int64
	PreviewLines int
	PerOutput    int64
	Total        int64
	Retention    time.Duration
}

type StoredOutput struct {
	ID      string `json:"id"`
	Size    int64  `json:"size"`
	Preview string `json:"preview"`
}

type OutputStore struct {
	mu     sync.Mutex
	config OutputConfig
	total  int64
}

type processOutputStore struct{ store *OutputStore }

// NewProcessOutputStore adapts the private managed output store to the process
// runner without coupling the process package to tool implementations.
func NewProcessOutputStore(store *OutputStore) process.OutputStore {
	if store == nil {
		return nil
	}
	return processOutputStore{store: store}
}

func (a processOutputStore) Store(ctx context.Context, reader io.Reader) (process.StoredOutput, error) {
	stored, err := a.store.Store(ctx, reader)
	return process.StoredOutput{ID: stored.ID, Size: stored.Size, Preview: stored.Preview}, err
}

func NewOutputStore(config OutputConfig) (*OutputStore, error) {
	if config.Directory == "" || config.PreviewBytes <= 0 || config.PreviewLines <= 0 || config.PerOutput <= 0 || config.Total <= 0 || config.PerOutput > config.Total {
		return nil, errors.New("invalid output store configuration")
	}
	if err := os.MkdirAll(config.Directory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(config.Directory, 0o700); err != nil {
		return nil, err
	}
	s := &OutputStore{config: config}
	if err := s.cleanupLocked(time.Now()); err != nil {
		return nil, err
	}
	return s, nil
}

// Store streams r directly to a private managed file while enforcing quotas.
func (s *OutputStore) Store(ctx context.Context, r io.Reader) (StoredOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return StoredOutput{}, err
	}
	id, err := randomOutputID()
	if err != nil {
		return StoredOutput{}, err
	}
	path := filepath.Join(s.config.Directory, id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return StoredOutput{}, err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return StoredOutput{}, err
		}
		n, readErr := r.Read(buffer)
		if n > 0 {
			if size+int64(n) > s.config.PerOutput {
				return StoredOutput{}, errors.New("per-output quota exceeded")
			}
			if s.total+size+int64(n) > s.config.Total {
				return StoredOutput{}, errors.New("total output quota exceeded")
			}
			written, writeErr := f.Write(buffer[:n])
			size += int64(written)
			if writeErr != nil {
				return StoredOutput{}, writeErr
			}
			if written != n {
				return StoredOutput{}, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return StoredOutput{}, readErr
		}
	}
	if err := f.Close(); err != nil {
		return StoredOutput{}, err
	}
	preview, err := s.preview(path, size)
	if err != nil {
		return StoredOutput{}, err
	}
	s.total += size
	ok = true
	return StoredOutput{ID: id, Size: size, Preview: preview}, nil
}

func (s *OutputStore) Read(id string, offset, limit int64) ([]byte, error) {
	if !validOutputID(id) || offset < 0 || limit < 0 || limit > s.config.PerOutput {
		return nil, errors.New("invalid output read")
	}
	f, err := os.Open(filepath.Join(s.config.Directory, id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

func (s *OutputStore) Cleanup(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupLocked(now)
}

// Maintain removes a bounded number of expired managed outputs and stale
// output temporary files. Unknown files and directories are left untouched.
func (s *OutputStore) Maintain(now time.Time, staleAfter time.Duration, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := os.Open(s.config.Directory)
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	removed, inspected := 0, 0
	for inspected < limit {
		entries, readErr := directory.ReadDir(min(128, limit-inspected))
		for _, entry := range entries {
			inspected++
			if entry.IsDir() {
				continue
			}
			managed := validOutputID(entry.Name())
			temporary := strings.HasPrefix(entry.Name(), ".parrot-output-")
			if !managed && !temporary {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return removed, infoErr
			}
			expired := managed && s.config.Retention > 0 && now.Sub(info.ModTime()) > s.config.Retention
			stale := temporary && staleAfter > 0 && now.Sub(info.ModTime()) > staleAfter
			if !expired && !stale {
				continue
			}
			if err := os.Remove(filepath.Join(s.config.Directory, entry.Name())); err != nil {
				return removed, err
			}
			if managed {
				s.total -= info.Size()
				if s.total < 0 {
					s.total = 0
				}
			}
			removed++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return removed, readErr
		}
	}
	return removed, nil
}

func (s *OutputStore) cleanupLocked(now time.Time) error {
	entries, err := os.ReadDir(s.config.Directory)
	if err != nil {
		return err
	}
	s.total = 0
	for _, entry := range entries {
		if entry.IsDir() || !validOutputID(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if s.config.Retention > 0 && now.Sub(info.ModTime()) > s.config.Retention {
			if err := os.Remove(filepath.Join(s.config.Directory, entry.Name())); err != nil {
				return err
			}
			continue
		}
		s.total += info.Size()
	}
	return nil
}

func (s *OutputStore) preview(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	n := s.config.PreviewBytes
	if size <= n {
		b, err := io.ReadAll(io.LimitReader(f, n))
		return limitPreview(string(bytesToUTF8(b)), s.config.PreviewLines), err
	}
	headBytes := (n + 1) / 2
	tailBytes := n / 2
	head, err := io.ReadAll(io.LimitReader(f, headBytes))
	if err != nil {
		return "", err
	}
	if _, err := f.Seek(-tailBytes, io.SeekEnd); err != nil {
		return "", err
	}
	tail, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return "", err
	}
	text := string(bytesToUTF8(head)) + fmt.Sprintf("\n... %d bytes omitted ...\n", size-n) + string(bytesToUTF8(tail))
	return limitPreview(text, s.config.PreviewLines), nil
}

func bytesToUTF8(b []byte) []byte {
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return []byte(strings.ToValidUTF8(string(b), ""))
}

func limitPreview(text string, lines int) string {
	parts := strings.Split(text, "\n")
	if len(parts) <= lines {
		return text
	}
	if lines == 1 {
		return parts[0]
	}
	remaining := lines - 1
	head := (remaining + 1) / 2
	tail := remaining - head
	result := strings.Join(parts[:head], "\n") + "\n... lines omitted ..."
	if tail > 0 {
		result += "\n" + strings.Join(parts[len(parts)-tail:], "\n")
	}
	return result
}

func randomOutputID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validOutputID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
