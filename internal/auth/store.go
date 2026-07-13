package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

var ErrCredentialNotFound = errors.New("auth: credential not found")

const maxCredentialStoreBytes = 16 << 20

type Store interface {
	Get(context.Context, string) (Credential, error)
	Put(context.Context, string, Credential) error
	Delete(context.Context, string) error
	List(context.Context) ([]string, error)
}

type FileStore struct {
	path string
	mu   sync.Mutex
}

type storeFile struct {
	Version     int                   `json:"version"`
	Credentials map[string]Credential `json:"credentials"`
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (s *FileStore) Get(ctx context.Context, name string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	values, err := s.read()
	if err != nil {
		return Credential{}, err
	}
	value, ok := values[name]
	if !ok {
		return Credential{}, ErrCredentialNotFound
	}
	return cloneCredential(value), nil
}

func (s *FileStore) Put(ctx context.Context, name string, value Credential) error {
	if name == "" {
		return errors.New("auth: credential name is empty")
	}
	if err := value.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	values, err := s.read()
	if err != nil {
		return err
	}
	values[name] = cloneCredential(value)
	return s.write(values)
}

func (s *FileStore) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	values, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := values[name]; !ok {
		return ErrCredentialNotFound
	}
	delete(values, name)
	return s.write(values)
}

func (s *FileStore) List(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values, err := s.read()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *FileStore) read() (map[string]Credential, error) {
	if s.path == "" {
		return nil, errors.New("auth: credential store path is empty")
	}
	handle, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]Credential), nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read credential store: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(handle, maxCredentialStoreBytes+1))
	closeErr := handle.Close()
	if readErr != nil {
		return nil, fmt.Errorf("auth: read credential store: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("auth: close credential store: %w", closeErr)
	}
	if len(data) > maxCredentialStoreBytes {
		return nil, errors.New("auth: credential store exceeds byte limit")
	}
	var file storeFile
	if err := strictJSON(data, &file); err != nil {
		return nil, fmt.Errorf("auth: malformed credential store: %w", err)
	}
	if file.Version != CredentialVersion || file.Credentials == nil {
		return nil, errors.New("auth: malformed credential store")
	}
	for _, value := range file.Credentials {
		if err := value.validate(); err != nil {
			return nil, fmt.Errorf("auth: malformed credential store: %w", err)
		}
	}
	return file.Credentials, nil
}

func (s *FileStore) write(values map[string]Credential) (err error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create credential directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("auth: restrict credential directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("auth: create temporary credential store: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		temp.Close()
		os.Remove(tempName)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("auth: restrict temporary credential store: %w", err)
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(storeFile{Version: CredentialVersion, Credentials: values}); err != nil {
		return fmt.Errorf("auth: encode credential store: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("auth: sync credential store: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("auth: close credential store: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("auth: replace credential store: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("auth: restrict credential store: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("auth: open credential directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("auth: sync credential directory: %w", err)
	}
	return nil
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
