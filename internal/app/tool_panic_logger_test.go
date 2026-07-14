package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
)

func TestToolPanicLoggerWritesStructuredRecord(t *testing.T) {
	state := t.TempDir()
	run, err := diagnostics.Start(state, diagnostics.Build{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	hook := toolPanicLogger()
	hook(context.Background(), "ses_123", "apply_patch", "boom", []byte("goroutine 1 [running]:\nmain.x()"))
	run.Finish(0)

	data, err := os.ReadFile(filepath.Join(state, "diagnostics", "parrot.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	record := string(data)
	for _, want := range []string{
		`"event":"panic_recovered"`, `"component":"agent_tool_call"`,
		`"session_id":"ses_123"`, `"tool":"apply_patch"`,
		`"panic":"boom"`, `goroutine 1 [running]`,
	} {
		if !strings.Contains(record, want) {
			t.Fatalf("diagnostics record missing %q, got: %s", want, record)
		}
	}
}
