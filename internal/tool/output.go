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
	ID           string `json:"id"`
	Size         int64  `json:"size"`
	OmittedBytes int64  `json:"omitted_bytes,omitempty"`
	Preview      string `json:"preview"`
}

type OutputStore struct {
	mu     sync.Mutex
	config OutputConfig
	total  int64
	active map[string]*managedOutput
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

func (a processOutputStore) Create(ctx context.Context) (process.ManagedOutput, error) {
	out, err := a.store.Create(ctx)
	if err != nil {
		return nil, err
	}
	return &processManagedOutput{managed: out}, nil
}

func (a processOutputStore) Read(id string, offset, limit int64) ([]byte, error) {
	return a.store.Read(id, offset, limit)
}

type processManagedOutput struct{ managed ManagedOutput }

func (o *processManagedOutput) Write(p []byte) (int, error) { return o.managed.Write(p) }
func (o *processManagedOutput) ID() string                  { return o.managed.ID() }
func (o *processManagedOutput) Finalize(ctx context.Context) (process.StoredOutput, error) {
	stored, err := o.managed.Finalize(ctx)
	return process.StoredOutput{ID: stored.ID, Size: stored.Size, OmittedBytes: stored.OmittedBytes, Preview: stored.Preview}, err
}
func (o *processManagedOutput) Discard() { o.managed.Discard() }

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
	s := &OutputStore{config: config, active: make(map[string]*managedOutput)}
	if err := s.cleanupLocked(time.Now()); err != nil {
		return nil, err
	}
	return s, nil
}

// ManagedOutput is a writable output handle that exposes an ID immediately and
// returns a StoredOutput on Finalize. The output may be read with Read while it
// is still being written.
type ManagedOutput interface {
	io.Writer
	ID() string
	Finalize(ctx context.Context) (StoredOutput, error)
	Discard()
}

// Store writes all data from r into a new managed output and finalizes it.
func (s *OutputStore) Store(ctx context.Context, r io.Reader) (StoredOutput, error) {
	out, err := s.Create(ctx)
	if err != nil {
		return StoredOutput{}, err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Discard()
		return StoredOutput{}, err
	}
	return out.Finalize(context.WithoutCancel(ctx))
}

// Create starts a new managed output and returns a writable handle. The file
// is created immediately so read_output can access it before Finalize is called.
func (s *OutputStore) Create(ctx context.Context) (ManagedOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := randomOutputID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.config.Directory, id)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	out := &managedOutput{store: s, id: id, path: path, file: f}
	s.mu.Lock()
	s.active[id] = out
	s.mu.Unlock()
	return out, nil
}

type managedOutput struct {
	store   *OutputStore
	mu      sync.Mutex
	id      string
	path    string
	file    *os.File
	size    int64
	omitted int64
	closed  bool
}

func (o *managedOutput) ID() string { return o.id }

func (o *managedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errors.New("managed output is closed")
	}
	written := 0
	const bufferSize = 32 * 1024
	for written < len(p) {
		chunkSize := len(p) - written
		if chunkSize > bufferSize {
			chunkSize = bufferSize
		}
		chunk := p[written : written+chunkSize]
		if err := o.writeChunk(chunk); err != nil {
			return written, err
		}
		written += chunkSize
	}
	return written, nil
}

func (o *managedOutput) writeChunk(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	o.store.mu.Lock()
	if o.store.total+o.size+int64(len(p)) > o.store.config.Total {
		o.store.mu.Unlock()
		return errors.New("total output quota exceeded")
	}
	o.store.mu.Unlock()
	n, err := o.file.Write(p)
	o.size += int64(n)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	if o.size > 2*o.store.config.PerOutput {
		if err := o.compact(); err != nil {
			return err
		}
	}
	return nil
}

func (o *managedOutput) compact() error {
	keep := o.store.config.PerOutput
	f, err := o.store.compactOutput(o.path, o.file, keep)
	if err != nil {
		return err
	}
	o.omitted += o.size - keep
	o.size = keep
	o.file = f
	return nil
}

func (o *managedOutput) Finalize(ctx context.Context) (StoredOutput, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return StoredOutput{}, errors.New("managed output already finalized")
	}
	o.closed = true
	if err := ctx.Err(); err != nil {
		o.remove()
		return StoredOutput{}, err
	}
	if o.size > o.store.config.PerOutput {
		if err := o.compact(); err != nil {
			o.remove()
			return StoredOutput{}, err
		}
	}
	if err := o.file.Close(); err != nil {
		return StoredOutput{}, err
	}
	preview, err := o.store.preview(o.path, o.size)
	if err != nil {
		return StoredOutput{}, err
	}
	o.store.mu.Lock()
	delete(o.store.active, o.id)
	o.store.total += o.size
	o.store.mu.Unlock()
	return StoredOutput{ID: o.id, Size: o.size, OmittedBytes: o.omitted, Preview: preview}, nil
}

func (o *managedOutput) Discard() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	o.remove()
}

func (o *managedOutput) remove() {
	_ = o.file.Close()
	_ = os.Remove(o.path)
	o.store.mu.Lock()
	delete(o.store.active, o.id)
	o.store.mu.Unlock()
}

// compactOutput rewrites the file at path keeping only its last keep bytes and
// returns a new append handle for the compacted file.
func (s *OutputStore) compactOutput(path string, f *os.File, keep int64) (*os.File, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	dropped := size - keep
	if dropped <= 0 {
		return f, nil
	}
	if _, err := f.Seek(dropped, io.SeekStart); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(s.config.Directory, ".parrot-output-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	_, copyErr := io.Copy(tmp, io.LimitReader(f, keep))
	closeErr := tmp.Close()
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return nil, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return nil, closeErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
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
			name := entry.Name()
			managed := validOutputID(name)
			temporary := strings.HasPrefix(name, ".parrot-output-")
			if !managed && !temporary {
				continue
			}
			if managed && s.active[name] != nil {
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
			if err := os.Remove(filepath.Join(s.config.Directory, name)); err != nil {
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
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".parrot-output-") {
			continue
		}
		if !validOutputID(name) {
			continue
		}
		if s.active[name] != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if s.config.Retention > 0 && now.Sub(info.ModTime()) > s.config.Retention {
			if err := os.Remove(filepath.Join(s.config.Directory, name)); err != nil {
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
