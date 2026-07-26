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
	"github.com/amirulashraf/parrot-coder/internal/id"
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
	Monitored   bool   `json:"monitored,omitempty"`
}

type Notification struct {
	ID   string
	Name string
	Item string
}

type metadata struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Monitored   bool   `json:"monitored,omitempty"`
	DeliveryID  string `json:"delivery_id,omitempty"`
}

// Store owns queues rooted in one absolute directory. Operations are serialized
// within the Store and by filesystem locks shared with other Store instances.
type Store struct {
	directory string
	mu        sync.Mutex
}

// New provisions directory and returns a Store bound to it.
func New(directory string) (*Store, error) {
	if directory == "" || !filepath.IsAbs(directory) {
		return nil, errors.New("queue: absolute directory is required")
	}
	directory = filepath.Clean(directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("queue: create directory: %w", err)
	}
	stat, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("queue: inspect directory: %w", err)
	}
	if !stat.IsDir() {
		return nil, errors.New("queue: path is not a directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("queue: secure directory: %w", err)
	}
	return &Store{directory: directory}, nil
}

// Directory returns the absolute directory containing the Store's queues.
func (s *Store) Directory() string { return s.directory }

// Create explicitly creates an empty queue.
func (s *Store) Create(name, description string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(name)
	if err != nil {
		return Info{}, err
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
func (s *Store) Push(name, item string, direction Direction) (Info, error) {
	if direction == "" {
		direction = Back
	}
	if direction != Front && direction != Back {
		return Info{}, ErrInvalidDirection
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(name)
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
		meta.DeliveryID = ""
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
func (s *Store) Take(name string, direction Direction) (string, Info, error) {
	if direction == "" {
		direction = Front
	}
	if direction != Front && direction != Back {
		return "", Info{}, ErrInvalidDirection
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(name)
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
	if index == 0 {
		meta.DeliveryID = ""
	}
	if err := write(path, meta, items); err != nil {
		return "", Info{}, err
	}
	return item, info(path, meta, len(items)), nil
}

// Monitor enables or disables idle notification delivery for a queue.
func (s *Store) Monitor(name string, enabled bool) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(name)
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
	meta.Monitored = enabled
	if err := write(path, meta, items); err != nil {
		return Info{}, err
	}
	return info(path, meta, len(items)), nil
}

// DeliverMonitored offers the oldest item from the first non-empty monitored
// queue in canonical name order to deliver. The item is removed only when
// deliver accepts it, while the queue remains locked against other consumers.
// Monitoring remains enabled after delivery.
func (s *Store) DeliverMonitored(deliver func(Notification) (bool, error)) (bool, error) {
	if deliver == nil {
		return false, errors.New("queue: delivery callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.directory)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("queue: list monitored: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !validName(name) {
			return false, fmt.Errorf("queue: malformed filename %q: %w", entry.Name(), ErrInvalidName)
		}
		path := filepath.Join(s.directory, entry.Name())
		release, err := lock(path)
		if err != nil {
			return false, err
		}
		meta, items, readErr := read(path)
		if readErr == nil && meta.Name != name {
			readErr = errors.New("queue: metadata name does not match path")
		}
		if readErr != nil {
			release()
			return false, fmt.Errorf("queue: read %q: %w", name, readErr)
		}
		if !meta.Monitored || len(items) == 0 {
			release()
			continue
		}
		if meta.DeliveryID == "" {
			meta.DeliveryID, err = id.New("qnt")
			if err != nil {
				release()
				return false, err
			}
			if err := write(path, meta, items); err != nil {
				release()
				return false, err
			}
		}
		accepted, deliverErr := deliver(Notification{ID: meta.DeliveryID, Name: name, Item: items[0]})
		if deliverErr != nil || !accepted {
			release()
			return false, deliverErr
		}
		meta.DeliveryID = ""
		writeErr := write(path, meta, items[1:])
		release()
		if writeErr != nil {
			return false, writeErr
		}
		return true, nil
	}
	return false, nil
}

// Get returns queue metadata and size without changing it.
func (s *Store) Get(name string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(name)
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

// List returns every queue ordered by canonical name.
func (s *Store) List() ([]Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.directory)
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
		path := filepath.Join(s.directory, entry.Name())
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

func (s *Store) path(name string) (string, error) {
	if !validName(name) {
		return "", ErrInvalidName
	}
	return filepath.Join(s.directory, name+".jsonl"), nil
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
	return Info{Path: path, Name: meta.Name, Description: meta.Description, Size: size, Monitored: meta.Monitored}
}
