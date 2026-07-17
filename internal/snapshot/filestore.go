package snapshot

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/atomicfile"
)

// Layout below the snapshot root.
const (
	blobsDir     = "blobs"
	journalsDir  = "snapshots"
	journalKey   = "journal.jsonl"
	cursorKey    = "cursor.json"
	recordVerson = 1
)

// fileStore keeps undo history as files rather than database rows.
//
// Blob contents are addressed by hash, so a name and its bytes are the same
// statement: two writers producing the same name necessarily wrote the same
// content, and a blob is never modified once published. That is what lets the
// store sit on a filesystem shared with other machines without any locking. The
// journal is per session and appended only by the process running that session.
type fileStore struct{ root string }

type journalState struct {
	Exists        bool   `json:"exists"`
	Mode          uint32 `json:"mode"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Size          int64  `json:"size,omitempty"`
}

type journalEntry struct {
	Path   string       `json:"path"`
	Before journalState `json:"before"`
	After  journalState `json:"after"`
}

type journalRecord struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	Workspace string         `json:"workspace"`
	SessionID string         `json:"session_id"`
	Position  int            `json:"position"`
	CreatedAt string         `json:"created_at"`
	Entries   []journalEntry `json:"entries"`
}

type cursorFile struct {
	Version  int    `json:"version"`
	Position int    `json:"position"`
	Updated  string `json:"updated_at"`
}

func (s fileStore) blobPath(digest string) string {
	return filepath.Join(s.root, blobsDir, digest[:2], digest)
}

func (s fileStore) sessionDir(sessionID string) string {
	return filepath.Join(s.root, journalsDir, sessionID)
}

func (s fileStore) journalPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), journalKey)
}

func (s fileStore) cursorPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), cursorKey)
}

// putBlob publishes content under its hash. Publishing an existing blob is a
// no-op because the name already asserts the bytes.
func (s fileStore) putBlob(digest string, data []byte) error {
	if digest == "" {
		return errors.New("snapshot: blob hash is required")
	}
	path := s.blobPath(digest)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("snapshot: create blob directory: %w", err)
	}
	return atomicfile.Write(path, data)
}

func (s fileStore) getBlob(digest string) ([]byte, error) {
	data, err := os.ReadFile(s.blobPath(digest))
	if err != nil {
		return nil, fmt.Errorf("snapshot: read blob %s: %w", digest, err)
	}
	return data, nil
}

func (s fileStore) hasBlob(digest string) bool {
	_, err := os.Stat(s.blobPath(digest))
	return err == nil
}

// records reads a session's journal.
//
// A trailing partial line is dropped rather than failing the read. A record is
// written by one process with a single append, so the only way to observe half
// of one is a crash mid-write; the completed records before it remain valid and
// are more useful than an error.
func (s fileStore) records(sessionID string) ([]journalRecord, error) {
	file, err := os.Open(s.journalPath(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot: open journal: %w", err)
	}
	defer file.Close()

	var result []journalRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record journalRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			// A torn tail is expected after a crash; anything earlier is not.
			break
		}
		if record.Version != recordVerson {
			break
		}
		result = append(result, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: read journal: %w", err)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Position < result[j].Position })
	return result, nil
}

// appendRecord adds one transaction to the end of a session's journal.
func (s fileStore) appendRecord(sessionID string, record journalRecord) error {
	record.Version = recordVerson
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("snapshot: encode journal record: %w", err)
	}
	if err := os.MkdirAll(s.sessionDir(sessionID), 0o700); err != nil {
		return fmt.Errorf("snapshot: create journal directory: %w", err)
	}
	file, err := os.OpenFile(s.journalPath(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot: open journal: %w", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("snapshot: append journal record: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("snapshot: sync journal: %w", err)
	}
	return file.Close()
}

// rewrite replaces a session's journal, used to drop a redo branch.
func (s fileStore) rewrite(sessionID string, records []journalRecord) error {
	var buffer []byte
	for _, record := range records {
		record.Version = recordVerson
		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("snapshot: encode journal record: %w", err)
		}
		buffer = append(append(buffer, line...), '\n')
	}
	if err := os.MkdirAll(s.sessionDir(sessionID), 0o700); err != nil {
		return fmt.Errorf("snapshot: create journal directory: %w", err)
	}
	return atomicfile.Write(s.journalPath(sessionID), buffer)
}

func (s fileStore) cursor(sessionID string) (int, error) {
	var file cursorFile
	err := atomicfile.ReadJSON(s.cursorPath(sessionID), &file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return file.Position, nil
}

func (s fileStore) setCursor(sessionID string, position int) error {
	if err := os.MkdirAll(s.sessionDir(sessionID), 0o700); err != nil {
		return fmt.Errorf("snapshot: create journal directory: %w", err)
	}
	return atomicfile.WriteJSON(s.cursorPath(sessionID), cursorFile{
		Version:  recordVerson,
		Position: position,
		Updated:  time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// removeSession deletes a session's undo history. Blobs are left to the sweep,
// since another session may reference the same content.
func (s fileStore) removeSession(sessionID string) error {
	return os.RemoveAll(s.sessionDir(sessionID))
}

// referenced collects every blob hash a session's journal depends on.
func referenced(records []journalRecord) map[string]int64 {
	result := make(map[string]int64)
	for _, record := range records {
		for _, entry := range record.Entries {
			for _, state := range []journalState{entry.Before, entry.After} {
				if state.Exists && state.SHA256 != "" {
					result[state.SHA256] = state.Size
				}
			}
		}
	}
	return result
}
