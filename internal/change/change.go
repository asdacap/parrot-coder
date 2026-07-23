// Package change plans and commits exact, hash-bound workspace mutations.
package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

var (
	ErrStale        = errors.New("change: file hash mismatch")
	ErrConflict     = errors.New("change: edit does not match exactly once")
	ErrInvalidPatch = errors.New("change: invalid patch")
)

type Config struct {
	MaxFileBytes int64
	// InjectFailure is a test and crash-recovery seam. It is called after each
	// committed filesystem operation with a one-based operation index.
	InjectFailure func(index int, path string) error
}

type Service struct{ config Config }

func NewService(config Config) *Service {
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = 16 << 20
	}
	return &Service{config: config}
}

type FileState struct {
	Path          string
	Exists        bool
	Mode          os.FileMode
	SymlinkTarget string
	Data          []byte
	SHA256        string
}

func (s FileState) clone() FileState {
	s.Data = append([]byte(nil), s.Data...)
	return s
}

type Mutation struct {
	RequestedPath string
	Path          string
	Before        FileState
	After         FileState
}

type Plan struct {
	Mutations   []Mutation
	Directories []string
	Diff        string
}

func (p Plan) Before() []FileState {
	out := make([]FileState, len(p.Mutations))
	for i := range p.Mutations {
		out[i] = p.Mutations[i].Before.clone()
	}
	return out
}

func (p Plan) After() []FileState {
	out := make([]FileState, len(p.Mutations))
	for i := range p.Mutations {
		out[i] = p.Mutations[i].After.clone()
	}
	return out
}

func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Service) readState(path string) (FileState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return absentState(path), nil
	}
	if err != nil {
		return FileState{}, err
	}
	state := FileState{Path: path, Exists: true, Mode: info.Mode()}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return FileState{}, err
		}
		state.SymlinkTarget = target
		state.Data = []byte(target)
		state.SHA256 = SHA256(append([]byte("symlink\x00"), state.Data...))
		return state, nil
	}
	if !info.Mode().IsRegular() {
		return FileState{}, errors.New("change: unsupported file type")
	}
	file, err := os.Open(path)
	if err != nil {
		return FileState{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, s.config.MaxFileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return FileState{}, readErr
	}
	if closeErr != nil {
		return FileState{}, closeErr
	}
	if int64(len(data)) > s.config.MaxFileBytes {
		return FileState{}, errors.New("change: file byte limit exceeded")
	}
	state.Data = data
	state.SHA256 = SHA256(data)
	return state, nil
}

func absentState(path string) FileState { return FileState{Path: path} }

func regularState(path string, data []byte, mode os.FileMode) FileState {
	return FileState{Path: path, Exists: true, Mode: mode.Perm(), Data: append([]byte(nil), data...), SHA256: SHA256(data)}
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func requireExistingParent(path string) error {
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("change: destination parent is not a directory")
	}
	return nil
}

func sameState(a, b FileState) bool {
	return a.Path == b.Path && a.Exists == b.Exists && (!a.Exists || a.Mode == b.Mode && a.SymlinkTarget == b.SymlinkTarget && a.SHA256 == b.SHA256)
}

// Commit revalidates all paths and preimages, creates planned parent
// directories, stages every regular output, then applies mutations in path
// order. Any failure removes created directories and restores the preimage.
func (s *Service) Commit(ctx context.Context, ws *workspace.Workspace, plan Plan) error {
	if ws == nil || len(plan.Mutations) == 0 {
		return errors.New("change: non-empty plan and workspace are required")
	}
	mutations := append([]Mutation(nil), plan.Mutations...)
	sort.Slice(mutations, func(i, j int) bool { return mutations[i].Path < mutations[j].Path })
	for _, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.revalidate(ws, mutation); err != nil {
			return err
		}
	}
	for i := 1; i < len(mutations); i++ {
		if mutations[i-1].Path == mutations[i].Path {
			return errors.New("change: multiple operations resolve to the same canonical path")
		}
	}
	directories := append([]string(nil), plan.Directories...)
	sort.Slice(directories, func(i, j int) bool {
		return pathDepth(directories[i]) < pathDepth(directories[j])
	})
	for _, directory := range directories {
		relative, err := filepath.Rel(ws.Root(), directory)
		if err != nil {
			return ErrStale
		}
		resolved, err := ws.ResolveCreate(relative)
		if err != nil || resolved != directory {
			return ErrStale
		}
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			return ErrStale
		}
	}
	createdDirectories := make([]string, 0, len(directories))
	for _, directory := range directories {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return errors.Join(err, removeDirectories(createdDirectories))
		}
		createdDirectories = append(createdDirectories, directory)
	}

	temps := make(map[string]string)
	discardTemps := func() error {
		var result error
		for mutationPath, tempPath := range temps {
			if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
			delete(temps, mutationPath)
		}
		return result
	}
	defer func() { _ = discardTemps() }()
	for _, mutation := range mutations {
		if !mutation.After.Exists || mutation.After.SymlinkTarget != "" {
			continue
		}
		temp, err := stageFile(mutation.Path, mutation.After.Data, mutation.After.Mode)
		if err != nil {
			return errors.Join(err, discardTemps(), removeDirectories(createdDirectories))
		}
		temps[mutation.Path] = temp
	}

	applied := make([]Mutation, 0, len(mutations))
	for i, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, s.rollback(applied), discardTemps(), removeDirectories(createdDirectories))
		}
		if err := applyState(mutation.After, temps[mutation.Path]); err != nil {
			return errors.Join(err, s.rollback(applied), discardTemps(), removeDirectories(createdDirectories))
		}
		delete(temps, mutation.Path)
		applied = append(applied, mutation)
		if s.config.InjectFailure != nil {
			if err := s.config.InjectFailure(i+1, mutation.Path); err != nil {
				return errors.Join(err, s.rollback(applied), discardTemps(), removeDirectories(createdDirectories))
			}
		}
	}
	if err := syncDirectories(mutations); err != nil {
		return errors.Join(err, s.rollback(applied), discardTemps(), removeDirectories(createdDirectories))
	}
	return nil
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func removeDirectories(directories []string) error {
	ordered := append([]string(nil), directories...)
	sort.Slice(ordered, func(i, j int) bool {
		return pathDepth(ordered[i]) > pathDepth(ordered[j])
	})
	var result error
	for _, directory := range ordered {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (s *Service) revalidate(ws *workspace.Workspace, mutation Mutation) error {
	var resolved string
	var err error
	if mutation.Before.Exists {
		resolved, err = ws.ResolveRead(mutation.RequestedPath)
	} else {
		resolved, err = ws.ResolveCreate(mutation.RequestedPath)
	}
	if err != nil || resolved != mutation.Path {
		return ErrStale
	}
	current, err := s.readState(mutation.Path)
	if err != nil || !sameState(current, mutation.Before) {
		return ErrStale
	}
	return nil
}

func (s *Service) rollback(applied []Mutation) error {
	var result error
	for i := len(applied) - 1; i >= 0; i-- {
		if err := restoreState(applied[i].Before); err != nil {
			result = errors.Join(result, fmt.Errorf("rollback %s: %w", applied[i].Path, err))
		}
	}
	return result
}

func stageFile(path string, data []byte, mode os.FileMode) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".parrot-change-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

func applyState(state FileState, staged string) error {
	if !state.Exists {
		if err := os.Remove(state.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if state.SymlinkTarget != "" {
		if err := os.Remove(state.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.Symlink(state.SymlinkTarget, state.Path)
	}
	if staged == "" {
		return errors.New("change: missing staged file")
	}
	return os.Rename(staged, state.Path)
}

func restoreState(state FileState) error {
	if !state.Exists {
		if err := os.Remove(state.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if state.SymlinkTarget != "" {
		return applyState(state, "")
	}
	temp, err := stageFile(state.Path, state.Data, state.Mode)
	if err != nil {
		return err
	}
	if err := applyState(state, temp); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func syncDirectories(mutations []Mutation) error {
	dirs := make(map[string]struct{})
	for _, mutation := range mutations {
		dirs[filepath.Dir(mutation.Path)] = struct{}{}
	}
	var names []string
	for dir := range dirs {
		names = append(names, dir)
	}
	sort.Strings(names)
	for _, dir := range names {
		file, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
