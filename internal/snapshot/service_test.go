package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

func snapshotHarness(t *testing.T, config Config) (context.Context, *Service, *workspace.Workspace, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "parrot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := session.NewService(db, event.NewRepository(db))
	created, err := sessions.Create(ctx, session.CreateParams{Title: "snapshots"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx, NewService(db, config), ws, created.ID
}

func TestUndoRedoModesSymlinksAndConflict(t *testing.T) {
	ctx, service, ws, sessionID := snapshotHarness(t, Config{})
	file := filepath.Join(ws.Root(), "file")
	link := filepath.Join(ws.Root(), "link")
	if err := os.WriteFile(file, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("first", link); err != nil {
		t.Fatal(err)
	}
	fileBefore, _ := service.Capture(file)
	linkBefore, _ := service.Capture(link)
	if err := os.WriteFile(file, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("second", link); err != nil {
		t.Fatal(err)
	}
	fileAfter, _ := service.Capture(file)
	linkAfter, _ := service.Capture(link)
	transaction, err := service.Record(ctx, ws, sessionID, []Entry{{Path: file, Before: fileBefore, After: fileAfter}, {Path: link, Before: linkBefore, After: linkAfter}})
	if err != nil || transaction.Position != 1 {
		t.Fatalf("record = %#v, %v", transaction, err)
	}
	if _, err := service.Undo(ctx, ws, sessionID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	info, _ := os.Stat(file)
	target, _ := os.Readlink(link)
	if string(data) != "before" || info.Mode().Perm() != 0o640 || target != "first" {
		t.Fatalf("undo file=%q mode=%o link=%q", data, info.Mode().Perm(), target)
	}

	if err := os.WriteFile(file, []byte("diverged"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Redo(ctx, ws, sessionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("redo conflict = %v", err)
	}
	target, _ = os.Readlink(link)
	if target != "first" {
		t.Fatal("conflicting redo partially mutated symlink")
	}
	if err := os.WriteFile(file, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Redo(ctx, ws, sessionID); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(file)
	target, _ = os.Readlink(link)
	if string(data) != "after" || target != "second" {
		t.Fatalf("redo file=%q link=%q", data, target)
	}
}

func TestUndoAtomicFailureAndRedoBranchClearing(t *testing.T) {
	injected := false
	ctx, service, ws, sessionID := snapshotHarness(t, Config{InjectFailure: func(index int, _ string) error {
		if injected && index == 2 {
			return errors.New("injected")
		}
		return nil
	}})
	var entries []Entry
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(ws.Root(), name)
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := service.Capture(path)
		if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, _ := service.Capture(path)
		entries = append(entries, Entry{Path: path, Before: before, After: after})
	}
	if _, err := service.Record(ctx, ws, sessionID, entries); err != nil {
		t.Fatal(err)
	}
	injected = true
	if _, err := service.Undo(ctx, ws, sessionID); err == nil {
		t.Fatal("injected undo succeeded")
	}
	for _, name := range []string{"a", "b"} {
		data, _ := os.ReadFile(filepath.Join(ws.Root(), name))
		if string(data) != "after" {
			t.Fatalf("%s partially restored: %q", name, data)
		}
	}
	injected = false
	if _, err := service.Undo(ctx, ws, sessionID); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(ws.Root(), "a")
	before, _ := service.Capture(path)
	if err := os.WriteFile(path, []byte("branch"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _ := service.Capture(path)
	if _, err := service.Record(ctx, ws, sessionID, []Entry{{Path: path, Before: before, After: after}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Redo(ctx, ws, sessionID); !errors.Is(err, ErrNoRedo) {
		t.Fatalf("redo branch was retained: %v", err)
	}
}

func TestQuotasAndOnlyMutatedFiles(t *testing.T) {
	ctx, service, ws, sessionID := snapshotHarness(t, Config{MaxBlobBytes: 8, MaxTransactions: 1})
	path := filepath.Join(ws.Root(), "file")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := service.Capture(path)
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _ := service.Capture(path)
	transaction, err := service.Record(ctx, ws, sessionID, []Entry{{Path: path, Before: before, After: after}, {Path: path + "-ignored", Before: State{Path: path + "-ignored"}, After: State{Path: path + "-ignored"}}})
	if err != nil || len(transaction.Entries) != 1 {
		t.Fatalf("record = %#v, %v", transaction, err)
	}
	if _, err := service.Record(ctx, ws, sessionID, []Entry{{Path: path, Before: after, After: before}}); !errors.Is(err, ErrConflict) {
		// Current equals after, but the supplied transaction's After is before,
		// so validation must fail before quota handling.
		t.Fatalf("invalid transaction error = %v", err)
	}
	if err := os.WriteFile(path, []byte("next"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, _ := service.Capture(path)
	if _, err := service.Record(ctx, ws, sessionID, []Entry{{Path: path, Before: after, After: next}}); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota error = %v", err)
	}
}

func TestRecordRejectsObsoleteBeforeState(t *testing.T) {
	ctx, service, ws, sessionID := snapshotHarness(t, Config{})
	path := filepath.Join(ws.Root(), "file")
	if err := os.WriteFile(path, []byte("zero"), 0o600); err != nil {
		t.Fatal(err)
	}
	zero, _ := service.Capture(path)
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	one, _ := service.Capture(path)
	if _, err := service.Record(ctx, ws, sessionID, []Entry{{Path: path, Before: zero, After: one}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Record(ctx, ws, sessionID, []Entry{{Path: path, Before: zero, After: one}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("obsolete before state error = %v", err)
	}
}
