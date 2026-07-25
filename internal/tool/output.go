package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/process"
	petname "github.com/dustinkirkland/golang-petname"
)

const largeOutputsDirectoryName = "large_outputs"

type OutputConfig struct {
	Directory    string
	PreviewBytes int64
	PreviewLines int
}

type StoredOutput struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Preview string `json:"preview"`
}

// OutputStore writes complete large outputs below each session's private state
// directory. Paths are returned so the ordinary read tool can inspect them.
type OutputStore struct{ config OutputConfig }

type processOutputStore struct{ store *OutputStore }

func NewProcessOutputStore(store *OutputStore) process.OutputStore {
	if store == nil {
		return nil
	}
	return processOutputStore{store: store}
}

func (a processOutputStore) Create(ctx context.Context, sessionID string) (process.Output, error) {
	out, err := a.store.Create(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return &processOutput{output: out}, nil
}

type processOutput struct{ output Output }

func (o *processOutput) Write(p []byte) (int, error) { return o.output.Write(p) }
func (o *processOutput) Path() string                { return o.output.Path() }
func (o *processOutput) Finalize(ctx context.Context) (process.StoredOutput, error) {
	stored, err := o.output.Finalize(ctx)
	return process.StoredOutput{Path: stored.Path, Size: stored.Size, Preview: stored.Preview}, err
}
func (o *processOutput) Discard() { o.output.Discard() }

func NewOutputStore(config OutputConfig) (*OutputStore, error) {
	if config.Directory == "" || !filepath.IsAbs(config.Directory) || config.PreviewBytes <= 0 || config.PreviewLines <= 0 {
		return nil, errors.New("invalid output store configuration")
	}
	return &OutputStore{config: config}, nil
}

type Output interface {
	io.Writer
	Path() string
	Finalize(context.Context) (StoredOutput, error)
	Discard()
}

func (s *OutputStore) Store(ctx context.Context, sessionID string, reader io.Reader) (StoredOutput, error) {
	out, err := s.Create(ctx, sessionID)
	if err != nil {
		return StoredOutput{}, err
	}
	if _, err := io.Copy(out, reader); err != nil {
		out.Discard()
		return StoredOutput{}, err
	}
	return out.Finalize(context.WithoutCancel(ctx))
}

func (s *OutputStore) Create(ctx context.Context, sessionID string) (Output, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validOutputSessionID(sessionID) {
		return nil, errors.New("invalid output session ID")
	}
	directory := filepath.Join(s.config.Directory, sessionID, largeOutputsDirectoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create large output directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("secure large output directory: %w", err)
	}
	for range 100 {
		path := filepath.Join(directory, petname.Generate(3, "-")+".txt")
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &largeOutput{store: s, path: path, file: file}, nil
	}
	return nil, errors.New("could not allocate a unique large output path")
}

type largeOutput struct {
	store  *OutputStore
	mu     sync.Mutex
	path   string
	file   *os.File
	size   int64
	closed bool
}

func (o *largeOutput) Path() string { return o.path }

func (o *largeOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errors.New("large output is closed")
	}
	n, err := o.file.Write(p)
	o.size += int64(n)
	return n, err
}

func (o *largeOutput) Finalize(ctx context.Context) (StoredOutput, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return StoredOutput{}, errors.New("large output already finalized")
	}
	o.closed = true
	if err := ctx.Err(); err != nil {
		o.remove()
		return StoredOutput{}, err
	}
	if err := o.file.Close(); err != nil {
		return StoredOutput{}, err
	}
	preview, err := o.store.preview(o.path, o.size)
	if err != nil {
		return StoredOutput{}, err
	}
	return StoredOutput{Path: o.path, Size: o.size, Preview: preview}, nil
}

func (o *largeOutput) Discard() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	o.remove()
}

func (o *largeOutput) remove() {
	_ = o.file.Close()
	_ = os.Remove(o.path)
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
	headBytes, tailBytes := (n+1)/2, n/2
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

func validOutputSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." &&
		!filepath.IsAbs(sessionID) && !strings.ContainsAny(sessionID, `/\\`)
}
