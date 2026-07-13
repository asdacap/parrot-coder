package change

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

func testWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestEditExactBOMCRLFAndStale(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	path := filepath.Join(ws.Root(), "file.txt")
	before := append([]byte{0xef, 0xbb, 0xbf}, []byte("one\r\ntwo\r\n")...)
	if err := os.WriteFile(path, before, 0o640); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	plan, err := service.PlanEdit(ctx, ws, Edit{Path: "file.txt", ExpectedSHA256: SHA256(before), Old: "two\n", New: "three\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plan.Mutations[0].After.Data, append([]byte{0xef, 0xbb, 0xbf}, []byte("one\r\nthree\r\n")...)) {
		t.Fatalf("newline/BOM changed: %q", plan.Mutations[0].After.Data)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if _, err := service.PlanEdit(ctx, ws, Edit{Path: "file.txt", ExpectedSHA256: SHA256(before), Old: "three", New: "four"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale hash error = %v", err)
	}
}

func TestEditMatchRulesAndExplicitCreation(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	data := []byte("same same")
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	base := Edit{Path: "file", ExpectedSHA256: SHA256(data), Old: "same", New: "new"}
	if _, err := service.PlanEdit(ctx, ws, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("multiple match error = %v", err)
	}
	base.Old = "missing"
	if _, err := service.PlanEdit(ctx, ws, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("zero match error = %v", err)
	}
	base.Old, base.ReplaceAll = "same", true
	plan, err := service.PlanEdit(ctx, ws, base)
	if err != nil || string(plan.Mutations[0].After.Data) != "new new" {
		t.Fatalf("replace all = %#v, %v", plan, err)
	}
	if _, err := service.PlanEdit(ctx, ws, Edit{Path: "new", New: "content"}); err == nil {
		t.Fatal("implicit creation accepted")
	}
	created, err := service.PlanEdit(ctx, ws, Edit{Path: "new", New: "content", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws, created); err != nil {
		t.Fatal(err)
	}
}

func TestPatchAllOperationsAndStrictRejections(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for name, data := range map[string]string{"update": "old\n", "delete": "gone\n", "move": "move\n"} {
		if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patch := "*** Begin Patch\n" +
		"*** Add File: added\n+new\n" +
		"*** Update File: update\n@@\n-old\n+changed\n" +
		"*** Delete File: delete\n" +
		"*** Update File: move\n*** Move to: moved\n@@\n-move\n+moved\n" +
		"*** End Patch\n"
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mutations) != 5 || !strings.Contains(plan.Diff, "+++ b/added") {
		t.Fatalf("plan = %#v", plan)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"added": "new\n", "update": "changed\n", "moved": "moved\n"} {
		got, err := os.ReadFile(filepath.Join(ws.Root(), name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"delete", "move"} {
		if _, err := os.Lstat(filepath.Join(ws.Root(), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists", name)
		}
	}

	malformed := []string{
		"prose\n*** Begin Patch\n*** End Patch",
		"*** Begin Patch\n*** Add File: ../escape\n+x\n*** End Patch",
		"*** Begin Patch\n*** Add File: x\nnot-prefixed\n*** End Patch",
		"*** Begin Patch\n*** Delete File: absent\ntext\n*** End Patch",
		"*** Begin Patch\n*** Update File: a\n*** Move to: b\n*** Update File: b\n*** Move to: a\n*** End Patch",
		"*** Begin Patch\n*** Add File: dir\n+x\n*** Add File: dir/file\n+y\n*** End Patch",
	}
	for _, input := range malformed {
		if _, err := ParsePatch(input); err == nil {
			t.Errorf("malformed patch accepted: %q", input)
		}
	}
	if _, err := service.PlanPatch(ctx, ws, "*** Begin Patch\n*** Add File: added\n+x\n*** End Patch"); err == nil {
		t.Fatal("existing add destination accepted")
	}
	if _, err := service.PlanPatch(ctx, ws, "*** Begin Patch\n*** Delete File: absent\n*** End Patch"); err == nil {
		t.Fatal("missing delete accepted")
	}
}

func TestPatchEndOfFileAnchorsDuplicateAndPreservesNoFinalNewline(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	data := []byte("same\r\nsame")
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: file\n@@\n-same\n+last\n*** End of File\n*** End Patch"
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plan.Mutations[0].After.Data); got != "same\r\nlast" {
		t.Fatalf("after = %q", got)
	}
}

func TestCommitRollbackAndSymlinkSwap(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patch := "*** Begin Patch\n*** Update File: a\n@@\n-old\n+new\n*** Update File: b\n@@\n-old\n+new\n*** End Patch"
	planner := NewService(Config{})
	plan, err := planner.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	failing := NewService(Config{InjectFailure: func(index int, _ string) error {
		if index == 2 {
			return errors.New("injected")
		}
		return nil
	}})
	if err := failing.Commit(ctx, ws, plan); err == nil {
		t.Fatal("injected failure succeeded")
	}
	for _, name := range []string{"a", "b"} {
		data, _ := os.ReadFile(filepath.Join(ws.Root(), name))
		if string(data) != "old\n" {
			t.Fatalf("%s not rolled back: %q", name, data)
		}
	}

	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		inside := filepath.Join(ws.Root(), "inside")
		link := filepath.Join(ws.Root(), "link")
		outside := filepath.Join(t.TempDir(), "outside")
		_ = os.WriteFile(inside, []byte("safe"), 0o600)
		_ = os.WriteFile(outside, []byte("secret"), 0o600)
		if err := os.Symlink("inside", link); err != nil {
			t.Fatal(err)
		}
		linkPlan, err := planner.PlanEdit(ctx, ws, Edit{Path: "link", ExpectedSHA256: SHA256([]byte("safe")), Old: "safe", New: "changed"})
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(link)
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := planner.Commit(ctx, ws, linkPlan); !errors.Is(err, ErrStale) {
			t.Fatalf("symlink swap error = %v", err)
		}
		data, _ := os.ReadFile(outside)
		if string(data) != "secret" {
			t.Fatal("outside file mutated")
		}
	}
}
