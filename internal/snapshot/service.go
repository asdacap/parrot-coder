// Package snapshot stores bounded file transaction journals and performs safe undo and redo.
package snapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
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

	"github.com/amirulashraf/parrot-coder/internal/store"
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
	db     *store.DB
	config Config
	mu     sync.Mutex
}

func NewService(db *store.DB, config Config) *Service {
	if config.MaxBlobBytes <= 0 {
		config.MaxBlobBytes = 256 << 20
	}
	if config.MaxTransactions <= 0 {
		config.MaxTransactions = 1000
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = 16 << 20
	}
	return &Service{db: db, config: config}
}

func (s *Service) Capture(path string) (State, error) {
	return capture(path, s.config.MaxFileBytes)
}

// Record appends one transaction at the current cursor. A new transaction
// after undo removes the entire redo branch in the same SQLite transaction.
func (s *Service) Record(ctx context.Context, ws *workspace.Workspace, sessionID string, entries []Entry) (Transaction, error) {
	if s == nil || s.db == nil || ws == nil || sessionID == "" {
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

	err = s.db.WithImmediate(ctx, func(tx *sql.Tx) error {
		cursor, err := loadCursor(ctx, tx, ws.Root(), sessionID)
		if err != nil {
			return err
		}
		for _, entry := range clean {
			latest, ok, err := latestJournalState(ctx, tx, ws.Root(), sessionID, cursor, entry.Path)
			if err != nil {
				return err
			}
			if ok && !stateEqual(latest, entry.Before) {
				return ErrConflict
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_transaction WHERE workspace=? AND session_id=? AND position>?`, ws.Root(), sessionID, cursor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_blob WHERE hash NOT IN (
			SELECT before_blob_hash FROM snapshot_file WHERE before_blob_hash IS NOT NULL
			UNION SELECT after_blob_hash FROM snapshot_file WHERE after_blob_hash IS NOT NULL
		)`); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_transaction`).Scan(&count); err != nil {
			return err
		}
		if count+1 > s.config.MaxTransactions {
			return ErrQuota
		}
		additional, err := additionalBlobBytes(ctx, tx, clean)
		if err != nil {
			return err
		}
		var existing int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM snapshot_blob`).Scan(&existing); err != nil {
			return err
		}
		if existing+additional > s.config.MaxBlobBytes {
			return ErrQuota
		}
		for _, entry := range clean {
			for _, state := range []State{entry.Before, entry.After} {
				if !state.Exists {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO snapshot_blob(hash,data,size) VALUES(?,?,?)`, state.SHA256, state.Data, len(state.Data)); err != nil {
					return err
				}
			}
		}
		result.Position = cursor + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_transaction(id,workspace,session_id,position,created_at) VALUES(?,?,?,?,?)`, id, ws.Root(), sessionID, result.Position, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		for i, entry := range clean {
			beforeBlob, beforeHash := nullableState(entry.Before)
			afterBlob, afterHash := nullableState(entry.After)
			if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_file(
				transaction_id,ordinal,path,before_exists,before_mode,before_symlink,before_hash,before_blob_hash,
				after_exists,after_mode,after_symlink,after_hash,after_blob_hash
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, i, entry.Path,
				boolInt(entry.Before.Exists), uint32(entry.Before.Mode), nullableString(entry.Before.SymlinkTarget), beforeHash, beforeBlob,
				boolInt(entry.After.Exists), uint32(entry.After.Mode), nullableString(entry.After.SymlinkTarget), afterHash, afterBlob); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO snapshot_cursor(workspace,session_id,position) VALUES(?,?,?)
			ON CONFLICT(workspace,session_id) DO UPDATE SET position=excluded.position`, ws.Root(), sessionID, result.Position)
		return err
	})
	if err != nil {
		return Transaction{}, fmt.Errorf("snapshot: record: %w", err)
	}
	return result, nil
}

func latestJournalState(ctx context.Context, tx *sql.Tx, root, sessionID string, cursor int, path string) (State, bool, error) {
	var state State
	var exists int
	var mode uint32
	err := tx.QueryRowContext(ctx, `SELECT f.after_exists,f.after_mode,COALESCE(f.after_symlink,''),COALESCE(f.after_hash,'')
		FROM snapshot_file f JOIN snapshot_transaction t ON t.id=f.transaction_id
		WHERE t.workspace=? AND t.session_id=? AND t.position<=? AND f.path=?
		ORDER BY t.position DESC LIMIT 1`, root, sessionID, cursor, path).Scan(&exists, &mode, &state.SymlinkTarget, &state.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	state.Path, state.Exists, state.Mode = path, exists == 1, os.FileMode(mode)
	return state, true, nil
}

func (s *Service) Undo(ctx context.Context, ws *workspace.Workspace, sessionID string) (Transaction, error) {
	return s.moveCursor(ctx, ws, sessionID, false)
}

func (s *Service) Redo(ctx context.Context, ws *workspace.Workspace, sessionID string) (Transaction, error) {
	return s.moveCursor(ctx, ws, sessionID, true)
}

func (s *Service) moveCursor(ctx context.Context, ws *workspace.Workspace, sessionID string, redo bool) (Transaction, error) {
	if s == nil || s.db == nil || ws == nil || sessionID == "" {
		return Transaction{}, errors.New("snapshot: service, workspace, and session are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, err := loadCursor(ctx, s.db.SQL(), ws.Root(), sessionID)
	if err != nil {
		return Transaction{}, err
	}
	position := cursor
	if redo {
		position++
	} else if position == 0 {
		return Transaction{}, ErrNoUndo
	}
	transaction, err := s.loadTransaction(ctx, ws.Root(), sessionID, position)
	if errors.Is(err, sql.ErrNoRows) {
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
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE snapshot_cursor SET position=? WHERE workspace=? AND session_id=?`, newCursor, ws.Root(), sessionID); err != nil {
		rollbackErr := s.restore(context.Background(), current)
		return Transaction{}, errors.Join(fmt.Errorf("snapshot: update cursor: %w", err), rollbackErr)
	}
	return transaction, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadCursor(ctx context.Context, db queryRower, root, sessionID string) (int, error) {
	var cursor int
	err := db.QueryRowContext(ctx, `SELECT position FROM snapshot_cursor WHERE workspace=? AND session_id=?`, root, sessionID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return cursor, err
}

func (s *Service) loadTransaction(ctx context.Context, root, sessionID string, position int) (Transaction, error) {
	var item Transaction
	var created string
	err := s.db.SQL().QueryRowContext(ctx, `SELECT id,workspace,session_id,position,created_at FROM snapshot_transaction WHERE workspace=? AND session_id=? AND position=?`, root, sessionID, position).Scan(&item.ID, &item.Workspace, &item.SessionID, &item.Position, &created)
	if err != nil {
		return Transaction{}, err
	}
	item.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Transaction{}, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT f.path,
		f.before_exists,f.before_mode,COALESCE(f.before_symlink,''),COALESCE(f.before_hash,''),COALESCE(bb.data,X''),
		f.after_exists,f.after_mode,COALESCE(f.after_symlink,''),COALESCE(f.after_hash,''),COALESCE(ab.data,X'')
		FROM snapshot_file f
		LEFT JOIN snapshot_blob bb ON bb.hash=f.before_blob_hash
		LEFT JOIN snapshot_blob ab ON ab.hash=f.after_blob_hash
		WHERE f.transaction_id=? ORDER BY f.ordinal`, item.ID)
	if err != nil {
		return Transaction{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry Entry
		var beforeExists, afterExists int
		var beforeMode, afterMode uint32
		if err := rows.Scan(&entry.Path, &beforeExists, &beforeMode, &entry.Before.SymlinkTarget, &entry.Before.SHA256, &entry.Before.Data,
			&afterExists, &afterMode, &entry.After.SymlinkTarget, &entry.After.SHA256, &entry.After.Data); err != nil {
			return Transaction{}, err
		}
		entry.Before.Path, entry.After.Path = entry.Path, entry.Path
		entry.Before.Exists, entry.After.Exists = beforeExists == 1, afterExists == 1
		entry.Before.Mode, entry.After.Mode = os.FileMode(beforeMode), os.FileMode(afterMode)
		item.Entries = append(item.Entries, entry)
	}
	return item, rows.Err()
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

func additionalBlobBytes(ctx context.Context, tx *sql.Tx, entries []Entry) (int64, error) {
	seen := make(map[string]int64)
	for _, entry := range entries {
		for _, state := range []State{entry.Before, entry.After} {
			if state.Exists {
				seen[state.SHA256] = int64(len(state.Data))
			}
		}
	}
	var total int64
	for digest, size := range seen {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_blob WHERE hash=?`, digest).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			total += size
		}
	}
	return total, nil
}

func nullableState(state State) (any, any) {
	if !state.Exists {
		return nil, nil
	}
	return state.SHA256, state.SHA256
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
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
