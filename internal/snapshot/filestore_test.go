package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

func newService(t *testing.T, config Config) (*Service, *workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	wsRoot := t.TempDir()
	ws, err := workspace.New(wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(root, config), ws, root
}

// write returns the entry describing creating path with content.
func writeEntry(t *testing.T, ws *workspace.Workspace, service *Service, name, content string) Entry {
	t.Helper()
	path := filepath.Join(ws.Root(), name)
	before, err := service.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := service.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{Path: path, Before: before, After: after}
}

func TestRecordThenUndoRedoRoundTrips(t *testing.T) {
	t.Parallel()
	service, ws, _ := newService(t, Config{})
	ctx := context.Background()

	entry := writeEntry(t, ws, service, "a.txt", "one")
	if _, err := service.Record(ctx, ws, "ses_1", []Entry{entry}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Undo(ctx, ws, "ses_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("undo did not remove the created file")
	}
	if _, err := service.Redo(ctx, ws, "ses_1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(entry.Path)
	if err != nil || string(data) != "one" {
		t.Fatalf("redo restored %q, %v", data, err)
	}
	if _, err := service.Redo(ctx, ws, "ses_1"); !errors.Is(err, ErrNoRedo) {
		t.Fatalf("second redo error = %v, want ErrNoRedo", err)
	}
}

// A record is one appended line, so a crash mid-write can only truncate the
// last one. Earlier records stay usable rather than failing the whole journal.
func TestTornJournalTailRecoversToLastCompleteRecord(t *testing.T) {
	t.Parallel()
	service, ws, root := newService(t, Config{})
	ctx := context.Background()

	if _, err := service.Record(ctx, ws, "ses_1", []Entry{writeEntry(t, ws, service, "a.txt", "one")}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Record(ctx, ws, "ses_1", []Entry{writeEntry(t, ws, service, "b.txt", "two")}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, journalsDir, "ses_1", journalKey)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the final record in half, as an interrupted append would.
	lines := strings.SplitAfter(string(data), "\n")
	torn := lines[0] + lines[1][:len(lines[1])/2]
	if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}

	records, err := fileStore{root: root}.records("ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Position != 1 {
		t.Fatalf("recovered %d records, want the first one only", len(records))
	}
}

// Quota is per session, so unrelated sessions cannot exhaust it, and hitting it
// must not cost the user the edit they asked for.
func TestQuotaIsPerSession(t *testing.T) {
	t.Parallel()
	service, ws, _ := newService(t, Config{MaxTransactions: 1})
	ctx := context.Background()

	if _, err := service.Record(ctx, ws, "ses_1", []Entry{writeEntry(t, ws, service, "a.txt", "one")}); err != nil {
		t.Fatal(err)
	}
	_, err := service.Record(ctx, ws, "ses_1", []Entry{writeEntry(t, ws, service, "b.txt", "two")})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("second record in the same session: %v, want ErrQuota", err)
	}
	// A different session has its own budget.
	if _, err := service.Record(ctx, ws, "ses_2", []Entry{writeEntry(t, ws, service, "c.txt", "three")}); err != nil {
		t.Fatalf("record in a second session: %v", err)
	}
}

func TestSweepKeepsReferencedAndRecentBlobs(t *testing.T) {
	t.Parallel()
	service, ws, root := newService(t, Config{})
	ctx := context.Background()

	if _, err := service.Record(ctx, ws, "ses_1", []Entry{writeEntry(t, ws, service, "a.txt", "one")}); err != nil {
		t.Fatal(err)
	}
	// An unreferenced blob, written just now.
	store := fileStore{root: root}
	if err := store.putBlob("ff00000000000000000000000000000000000000000000000000000000000000", []byte("junk")); err != nil {
		t.Fatal(err)
	}

	// Within the grace period nothing is removed, because another machine may
	// have written a blob whose record this process cannot see yet.
	removed, err := service.Sweep(time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("swept %d blobs inside the grace period, want 0", removed)
	}

	removed, err = service.Sweep(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("swept %d blobs, want the 1 unreferenced blob", removed)
	}
	// The referenced blob survives, so undo still works.
	if _, err := service.Undo(ctx, ws, "ses_1"); err != nil {
		t.Fatalf("undo after sweep: %v", err)
	}
}

// The store must never keep undo history in a database, which is what put it on
// a shared filesystem behind locks that do not work there.
func TestStoreWritesOnlyPlainFiles(t *testing.T) {
	t.Parallel()
	service, ws, root := newService(t, Config{})
	if _, err := service.Record(context.Background(), ws, "ses_1", []Entry{writeEntry(t, ws, service, "a.txt", "one")}); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, suffix := range []string{".db", "-wal", "-shm", "-journal"} {
			if strings.HasSuffix(path, suffix) {
				return errors.New("snapshot root contains a database artifact: " + path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
