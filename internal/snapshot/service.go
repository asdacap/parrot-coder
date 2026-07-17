// Package snapshot stores bounded file transaction journals and performs safe undo and redo.
package snapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

var (
	ErrConflict = errors.New("snapshot: current files conflict with journal")
	ErrNoUndo   = errors.New("snapshot: nothing to undo")
	ErrNoRedo   = errors.New("snapshot: nothing to redo")
	ErrQuota    = errors.New("snapshot: quota exceeded")
)

type Config struct {
	MaxBlobBytes    int64
	MaxTransactions int
	MaxFileBytes    int64
	InjectFailure   func(index int, path string) error
}

type State struct {
	Path          string
	Exists        bool
	Mode          os.FileMode
	SymlinkTarget string
	Data          []byte
	SHA256        string
}

type Entry struct {
	Path   string
	Before State
	After  State
}

type Transaction struct {
	ID        string
	Workspace string
	SessionID string
	Position  int
	CreatedAt time.Time
	Entries   []Entry
}

type Service struct {
	files  fileStore
	config Config
	mu     sync.Mutex
}

// NewService stores undo history under root. Quotas apply per session: the
// shared database counted every session's transactions against one global cap,
// so unrelated history from other projects could exhaust it permanently.
func NewService(root string, config Config) *Service {
	if config.MaxBlobBytes <= 0 {
		config.MaxBlobBytes = 256 << 20
	}
	if config.MaxTransactions <= 0 {
		config.MaxTransactions = 1000
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = 16 << 20
	}
	return &Service{files: fileStore{root: root}, config: config}
}

func (s *Service) Capture(path string) (State, error) {
	return capture(path, s.config.MaxFileBytes)
}

// Record appends one transaction at the current cursor. A new transaction
// after undo removes the entire redo branch in the same SQLite transaction.
func (s *Service) Record(ctx context.Context, ws *workspace.Workspace, sessionID string, entries []Entry) (Transaction, error) {
	if s == nil || s.files.root == "" || ws == nil || sessionID == "" {
		return Transaction{}, errors.New("snapshot: service, workspace, and session are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clean, err := s.validateEntries(ws, entries, true)
	if err != nil {
		return Transaction{}, err
	}
	if len(clean) == 0 {
		return Transaction{}, errors.New("snapshot: transaction has no mutations")
	}
	id, err := transactionID()
	if err != nil {
		return Transaction{}, err
	}
	now := time.Now().UTC()
	result := Transaction{ID: id, Workspace: ws.Root(), SessionID: sessionID, CreatedAt: now, Entries: cloneEntries(clean)}

	err = func() error {
		records, err := s.files.records(sessionID)
		if err != nil {
			return err
		}
		cursor, err := s.files.cursor(sessionID)
		if err != nil {
			return err
		}
		for _, entry := range clean {
			latest, ok := latestJournalState(records, ws.Root(), cursor, entry.Path)
			if ok && !stateEqual(latest, entry.Before) {
				return ErrConflict
			}
		}
		// A new transaction after an undo discards the redo branch it replaces.
		kept := make([]journalRecord, 0, len(records))
		for _, record := range records {
			if record.Position <= cursor {
				kept = append(kept, record)
			}
		}
		if len(kept)+1 > s.config.MaxTransactions {
			return ErrQuota
		}
		live := referenced(kept)
		var total int64
		for _, size := range live {
			total += size
		}
		for _, entry := range clean {
			for _, state := range []State{entry.Before, entry.After} {
				if state.Exists {
					if _, ok := live[state.SHA256]; !ok && !s.files.hasBlob(state.SHA256) {
						live[state.SHA256] = int64(len(state.Data))
						total += int64(len(state.Data))
					}
				}
			}
		}
		if total > s.config.MaxBlobBytes {
			return ErrQuota
		}
		if len(kept) != len(records) {
			if err := s.files.rewrite(sessionID, kept); err != nil {
				return err
			}
		}
		// Blobs are published before the record that names them, so a crash
		// leaves unreferenced content for the sweep rather than a record
		// pointing at bytes that were never written.
		for _, entry := range clean {
			for _, state := range []State{entry.Before, entry.After} {
				if !state.Exists {
					continue
				}
				if err := s.files.putBlob(state.SHA256, state.Data); err != nil {
					return err
				}
			}
		}
		result.Position = cursor + 1
		record := journalRecord{
			ID: id, Workspace: ws.Root(), SessionID: sessionID, Position: result.Position,
			CreatedAt: now.Format(time.RFC3339Nano), Entries: make([]journalEntry, len(clean)),
		}
		for i, entry := range clean {
			record.Entries[i] = journalEntry{Path: entry.Path, Before: toJournalState(entry.Before), After: toJournalState(entry.After)}
		}
		if err := s.files.appendRecord(sessionID, record); err != nil {
			return err
		}
		return s.files.setCursor(sessionID, result.Position)
	}()
	if err != nil {
		return Transaction{}, fmt.Errorf("snapshot: record: %w", err)
	}
	return result, nil
}

// latestJournalState reports the newest recorded state of a path at or before
// the cursor, which is what a new transaction must agree with.
func latestJournalState(records []journalRecord, root string, cursor int, path string) (State, bool) {
	var found journalState
	var ok bool
	for _, record := range records {
		if record.Workspace != root || record.Position > cursor {
			continue
		}
		for _, entry := range record.Entries {
			if entry.Path == path {
				found, ok = entry.After, true
			}
		}
	}
	if !ok {
		return State{}, false
	}
	return fromJournalState(path, found, nil), true
}

func toJournalState(state State) journalState {
	if !state.Exists {
		return journalState{}
	}
	return journalState{
		Exists:        true,
		Mode:          uint32(state.Mode),
		SymlinkTarget: state.SymlinkTarget,
		SHA256:        state.SHA256,
		Size:          int64(len(state.Data)),
	}
}

func fromJournalState(path string, state journalState, data []byte) State {
	if !state.Exists {
		return State{Path: path}
	}
	return State{
		Path:          path,
		Exists:        true,
		Mode:          os.FileMode(state.Mode),
		SymlinkTarget: state.SymlinkTarget,
		SHA256:        state.SHA256,
		Data:          data,
	}
}

func (s *Service) Undo(ctx context.Context, ws *workspace.Workspace, sessionID string) (Transaction, error) {
	return s.moveCursor(ctx, ws, sessionID, false)
}

func (s *Service) Redo(ctx context.Context, ws *workspace.Workspace, sessionID string) (Transaction, error) {
	return s.moveCursor(ctx, ws, sessionID, true)
}

func (s *Service) moveCursor(ctx context.Context, ws *workspace.Workspace, sessionID string, redo bool) (Transaction, error) {
	if s == nil || s.files.root == "" || ws == nil || sessionID == "" {
		return Transaction{}, errors.New("snapshot: service, workspace, and session are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, err := s.files.cursor(sessionID)
	if err != nil {
		return Transaction{}, err
	}
	position := cursor
	if redo {
		position++
	} else if position == 0 {
		return Transaction{}, ErrNoUndo
	}
	transaction, err := s.loadTransaction(sessionID, ws.Root(), position)
	if errors.Is(err, errNoTransaction) {
		if redo {
			return Transaction{}, ErrNoRedo
		}
		return Transaction{}, ErrNoUndo
	}
	if err != nil {
		return Transaction{}, err
	}
	expected := make([]State, len(transaction.Entries))
	target := make([]State, len(transaction.Entries))
	for i, entry := range transaction.Entries {
		if redo {
			expected[i], target[i] = entry.Before, entry.After
		} else {
			expected[i], target[i] = entry.After, entry.Before
		}
	}
	current, err := s.checkCurrent(ws, expected)
	if err != nil {
		return Transaction{}, err
	}
	if err := s.restore(ctx, target); err != nil {
		return Transaction{}, err
	}
	newCursor := position
	if !redo {
		newCursor = position - 1
	}
	if err := s.files.setCursor(sessionID, newCursor); err != nil {
		rollbackErr := s.restore(context.Background(), current)
		return Transaction{}, errors.Join(fmt.Errorf("snapshot: update cursor: %w", err), rollbackErr)
	}
	return transaction, nil
}

var errNoTransaction = errors.New("snapshot: no such transaction")

// loadTransaction rebuilds one transaction, reading each state's content back
// from the blob store.
func (s *Service) loadTransaction(sessionID, root string, position int) (Transaction, error) {
	records, err := s.files.records(sessionID)
	if err != nil {
		return Transaction{}, err
	}
	for _, record := range records {
		if record.Position != position || record.Workspace != root {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
		if err != nil {
			return Transaction{}, err
		}
		item := Transaction{ID: record.ID, Workspace: record.Workspace, SessionID: record.SessionID, Position: record.Position, CreatedAt: created}
		for _, entry := range record.Entries {
			before, err := s.loadState(entry.Path, entry.Before)
			if err != nil {
				return Transaction{}, err
			}
			after, err := s.loadState(entry.Path, entry.After)
			if err != nil {
				return Transaction{}, err
			}
			item.Entries = append(item.Entries, Entry{Path: entry.Path, Before: before, After: after})
		}
		return item, nil
	}
	return Transaction{}, errNoTransaction
}

func (s *Service) loadState(path string, state journalState) (State, error) {
	if !state.Exists {
		return State{Path: path}, nil
	}
	data, err := s.files.getBlob(state.SHA256)
	if err != nil {
		return State{}, err
	}
	return fromJournalState(path, state, data), nil
}

func (s *Service) validateEntries(ws *workspace.Workspace, entries []Entry, checkAfter bool) ([]Entry, error) {
	clean := make([]Entry, 0, len(entries))
	seen := make(map[string]struct{})
	for _, entry := range entries {
		path, err := securePath(ws.Root(), entry.Path)
		if err != nil {
			return nil, err
		}
		entry.Path, entry.Before.Path, entry.After.Path = path, path, path
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("snapshot: duplicate path %q", path)
		}
		seen[path] = struct{}{}
		if stateEqual(entry.Before, entry.After) {
			continue
		}
		if err := validateState(entry.Before, s.config.MaxFileBytes); err != nil {
			return nil, err
		}
		if err := validateState(entry.After, s.config.MaxFileBytes); err != nil {
			return nil, err
		}
		if checkAfter {
			current, err := capture(path, s.config.MaxFileBytes)
			if err != nil || !stateEqual(current, entry.After) {
				return nil, ErrConflict
			}
		}
		clean = append(clean, entry)
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Path < clean[j].Path })
	return clean, nil
}

func (s *Service) checkCurrent(ws *workspace.Workspace, expected []State) ([]State, error) {
	current := make([]State, len(expected))
	for i, state := range expected {
		path, err := securePath(ws.Root(), state.Path)
		if err != nil || path != state.Path {
			return nil, ErrConflict
		}
		current[i], err = capture(path, s.config.MaxFileBytes)
		if err != nil || !stateEqual(current[i], state) {
			return nil, ErrConflict
		}
	}
	return current, nil
}

func (s *Service) restore(ctx context.Context, states []State) error {
	ordered := append([]State(nil), states...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	rollback := make([]State, len(ordered))
	for i := range ordered {
		var err error
		rollback[i], err = capture(ordered[i].Path, s.config.MaxFileBytes)
		if err != nil {
			return err
		}
	}
	temps := make(map[string]string)
	defer func() {
		for _, temp := range temps {
			_ = os.Remove(temp)
		}
	}()
	for _, state := range ordered {
		if state.Exists && state.SymlinkTarget == "" {
			temp, err := stage(state)
			if err != nil {
				return err
			}
			temps[state.Path] = temp
		}
	}
	applied := 0
	for i, state := range ordered {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, restoreRollback(rollback[:applied]))
		}
		if err := apply(state, temps[state.Path]); err != nil {
			return errors.Join(err, restoreRollback(rollback[:applied]))
		}
		delete(temps, state.Path)
		applied++
		if s.config.InjectFailure != nil {
			if err := s.config.InjectFailure(i+1, state.Path); err != nil {
				return errors.Join(err, restoreRollback(rollback[:applied]))
			}
		}
	}
	return nil
}

func capture(path string, max int64) (State, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Path: path}, nil
	}
	if err != nil {
		return State{}, err
	}
	state := State{Path: path, Exists: true, Mode: info.Mode()}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return State{}, err
		}
		state.SymlinkTarget, state.Data = target, []byte(target)
		state.SHA256 = hash(append([]byte("symlink\x00"), state.Data...))
		return state, nil
	}
	if !info.Mode().IsRegular() {
		return State{}, errors.New("snapshot: unsupported file type")
	}
	file, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, max+1))
	closeErr := file.Close()
	if readErr != nil {
		return State{}, readErr
	}
	if closeErr != nil {
		return State{}, closeErr
	}
	if int64(len(data)) > max {
		return State{}, ErrQuota
	}
	state.Data, state.SHA256 = data, hash(data)
	return state, nil
}

func validateState(state State, max int64) error {
	if !state.Exists {
		if state.SHA256 != "" || len(state.Data) != 0 || state.SymlinkTarget != "" {
			return errors.New("snapshot: absent state contains data")
		}
		return nil
	}
	if int64(len(state.Data)) > max {
		return ErrQuota
	}
	want := hash(state.Data)
	if state.SymlinkTarget != "" {
		if string(state.Data) != state.SymlinkTarget {
			return errors.New("snapshot: symlink data mismatch")
		}
		want = hash(append([]byte("symlink\x00"), state.Data...))
	}
	if state.SHA256 != want {
		return errors.New("snapshot: state hash mismatch")
	}
	return nil
}

func stateEqual(a, b State) bool {
	return a.Exists == b.Exists && (!a.Exists || a.Mode == b.Mode && a.SymlinkTarget == b.SymlinkTarget && a.SHA256 == b.SHA256)
}

func securePath(root, path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("snapshot: invalid path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if !contained(root, path) {
		return "", errors.New("snapshot: path outside workspace")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || !contained(root, parent) {
		return "", errors.New("snapshot: path parent changed or escaped workspace")
	}
	return path, nil
}

func contained(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func transactionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "snp_" + hex.EncodeToString(b), nil
}

func cloneEntries(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	for i, entry := range entries {
		out[i] = entry
		out[i].Before.Data = append([]byte(nil), entry.Before.Data...)
		out[i].After.Data = append([]byte(nil), entry.After.Data...)
	}
	return out
}

func stage(state State) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(state.Path), ".parrot-snapshot-*")
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
	if _, err := file.Write(state.Data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Chmod(state.Mode.Perm()); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

func apply(state State, staged string) error {
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
		return errors.New("snapshot: missing staged file")
	}
	return os.Rename(staged, state.Path)
}

func restoreRollback(states []State) error {
	var result error
	for i := len(states) - 1; i >= 0; i-- {
		state := states[i]
		if state.Exists && state.SymlinkTarget == "" {
			temp, err := stage(state)
			if err == nil {
				err = apply(state, temp)
			}
			result = errors.Join(result, err)
			continue
		}
		result = errors.Join(result, apply(state, ""))
	}
	return result
}
