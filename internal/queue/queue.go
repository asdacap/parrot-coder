// Package queue persists session-scoped queues as JSON Lines files.
package queue

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/atomicfile"
)

var (
	ErrNotFound         = errors.New("queue: not found")
	ErrAlreadyExists    = errors.New("queue: already exists")
	ErrInvalidName      = errors.New("queue: name must be exactly three lowercase ASCII alphanumeric words separated by hyphens")
	ErrInvalidDirection = errors.New("queue: direction must be front or back")
	ErrEmpty            = errors.New("queue: empty")
)

// Direction identifies one end of a queue. An empty direction selects the
// operation's FIFO default: back for Push and front for Take.
type Direction string

const (
	Front Direction = "front"
	Back  Direction = "back"
)

// Info describes a queue and its current number of items.
type Info struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Size        int    `json:"size"`
}

type metadata struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Store owns queues rooted in a state directory. Operations are serialized
// within the Store and by filesystem locks shared with other Store instances.
type Store struct {
	state string
	mu    sync.Mutex
}

func New(state string) *Store { return &Store{state: state} }

// Create explicitly creates an empty queue for an existing session.
func (s *Store) Create(sessionID, name, description string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(sessionID, name)
	if err != nil {
		return Info{}, err
	}
	if _, err := os.Stat(sessionDir(s.state, sessionID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Info{}, fmt.Errorf("queue: session does not exist: %w", err)
		}
		return Info{}, fmt.Errorf("queue: locate session: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Info{}, fmt.Errorf("queue: create directory: %w", err)
	}
	data, err := encode(metadata{Name: name, Description: description}, nil)
	if err != nil {
		return Info{}, err
	}
	if err := create(path, data); errors.Is(err, fs.ErrExist) {
		return Info{}, ErrAlreadyExists
	} else if err != nil {
		return Info{}, err
	}
	return Info{Path: path, Name: name, Description: description}, nil
}

// Push inserts item at direction. The empty direction appends at the back.
func (s *Store) Push(sessionID, name, item string, direction Direction) (Info, error) {
	if direction == "" {
		direction = Back
	}
	if direction != Front && direction != Back {
		return Info{}, ErrInvalidDirection
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(sessionID, name)
	if err != nil {
		return Info{}, err
	}
	release, err := lock(path)
	if err != nil {
		return Info{}, err
	}
	defer release()
	meta, items, err := read(path)
	if err != nil {
		return Info{}, err
	}
	if meta.Name != name {
		return Info{}, errors.New("queue: metadata name does not match path")
	}
	if direction == Front {
		items = append([]string{item}, items...)
	} else {
		items = append(items, item)
	}
	if err := write(path, meta, items); err != nil {
		return Info{}, err
	}
	return info(path, meta, len(items)), nil
}

// Take removes and returns an item from direction. The empty direction removes
// from the front, making default Push and Take FIFO. Empty queues are retained.
func (s *Store) Take(sessionID, name string, direction Direction) (string, Info, error) {
	if direction == "" {
		direction = Front
	}
	if direction != Front && direction != Back {
		return "", Info{}, ErrInvalidDirection
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(sessionID, name)
	if err != nil {
		return "", Info{}, err
	}
	release, err := lock(path)
	if err != nil {
		return "", Info{}, err
	}
	defer release()
	meta, items, err := read(path)
	if err != nil {
		return "", Info{}, err
	}
	if meta.Name != name {
		return "", Info{}, errors.New("queue: metadata name does not match path")
	}
	if len(items) == 0 {
		return "", info(path, meta, 0), ErrEmpty
	}
	index := 0
	if direction == Back {
		index = len(items) - 1
	}
	item := items[index]
	items = append(items[:index], items[index+1:]...)
	if err := write(path, meta, items); err != nil {
		return "", Info{}, err
	}
	return item, info(path, meta, len(items)), nil
}

// Get returns queue metadata and size without changing it.
func (s *Store) Get(sessionID, name string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(sessionID, name)
	if err != nil {
		return Info{}, err
	}
	meta, items, err := read(path)
	if err != nil {
		return Info{}, err
	}
	if meta.Name != name {
		return Info{}, errors.New("queue: metadata name does not match path")
	}
	return info(path, meta, len(items)), nil
}

// List returns every queue in a session, ordered by canonical name. A session
// with no queues directory has an empty list.
func (s *Store) List(sessionID string) ([]Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, `/\\`) {
		return nil, errors.New("queue: valid session ID is required")
	}
	dir := filepath.Join(sessionDir(s.state, sessionID), "queues")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: list: %w", err)
	}
	result := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !validName(name) {
			return nil, fmt.Errorf("queue: malformed filename %q: %w", entry.Name(), ErrInvalidName)
		}
		path := filepath.Join(dir, entry.Name())
		meta, items, err := read(path)
		if err != nil {
			return nil, fmt.Errorf("queue: read %q: %w", name, err)
		}
		if meta.Name != name {
			return nil, fmt.Errorf("queue: metadata name %q does not match path %q", meta.Name, name)
		}
		result = append(result, info(path, meta, len(items)))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Store) path(sessionID, name string) (string, error) {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, `/\\`) {
		return "", errors.New("queue: valid session ID is required")
	}
	if !validName(name) {
		return "", ErrInvalidName
	}
	return filepath.Join(sessionDir(s.state, sessionID), "queues", name+".jsonl"), nil
}

func sessionDir(state, sessionID string) string {
	return filepath.Join(state, "session", sessionID)
}

// lock serializes read-modify-write operations across Store instances and
// processes. Directory creation is atomic on local filesystems and NFS.
func lock(path string) (func(), error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		} else if !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, fs.ErrNotExist) {
				return func() {}, nil
			}
			return nil, fmt.Errorf("queue: acquire lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, errors.New("queue: timed out acquiring lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func validName(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, c := range []byte(part) {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return false
			}
		}
	}
	return true
}

func read(path string) (metadata, []string, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return metadata{}, nil, ErrNotFound
	}
	if err != nil {
		return metadata{}, nil, fmt.Errorf("queue: open: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return metadata{}, nil, fmt.Errorf("queue: stat: %w", err)
	}
	if stat.Size() > atomicfile.MaxBytes {
		return metadata{}, nil, fmt.Errorf("queue: file exceeds %d bytes", atomicfile.MaxBytes)
	}
	reader := bufio.NewReader(file)
	var meta metadata
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return metadata{}, nil, fmt.Errorf("queue: read metadata: %w", err)
	}
	if err := decodeLine(line, &meta); err != nil {
		return metadata{}, nil, fmt.Errorf("queue: decode metadata: %w", err)
	}
	var items []string
	for {
		line, err = reader.ReadBytes('\n')
		if len(line) > 0 {
			var item string
			if decodeErr := decodeLine(line, &item); decodeErr != nil {
				return metadata{}, nil, fmt.Errorf("queue: decode item: %w", decodeErr)
			}
			items = append(items, item)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return metadata{}, nil, fmt.Errorf("queue: read: %w", err)
		}
	}
	return meta, items, nil
}

func decodeLine(line []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values on line")
	}
	return nil
}

func create(path string, data []byte) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("queue: stage create: %w", err)
	}
	name := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(name)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("queue: restrict staged create: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("queue: write staged create: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("queue: sync staged create: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("queue: close staged create: %w", err)
	}
	if err := os.Link(name, path); err != nil {
		return err
	}
	return nil
}

func write(path string, meta metadata, items []string) error {
	data, err := encode(meta, items)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(path, data); err != nil {
		return fmt.Errorf("queue: persist: %w", err)
	}
	return nil
}

func encode(meta metadata, items []string) ([]byte, error) {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	if err := encoder.Encode(meta); err != nil {
		return nil, fmt.Errorf("queue: encode metadata: %w", err)
	}
	for _, item := range items {
		if err := encoder.Encode(item); err != nil {
			return nil, fmt.Errorf("queue: encode item: %w", err)
		}
	}
	return data.Bytes(), nil
}

func info(path string, meta metadata, size int) Info {
	return Info{Path: path, Name: meta.Name, Description: meta.Description, Size: size}
}
