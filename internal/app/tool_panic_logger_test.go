package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolPanicLoggerNilSink(t *testing.T) {
	if toolPanicLogger(nil) != nil {
		t.Fatal("expected nil hook when no diagnostics sink is available")
	}
}

func TestToolPanicLoggerWritesRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), DiagnosticsFile)
	sink, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	hook := toolPanicLogger(sink)
	if hook == nil {
		t.Fatal("expected a hook for a valid sink")
	}
	hook(context.Background(), "ses_123", "apply_patch", "boom", []byte("goroutine 1 [running]:\nmain.x()"))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := string(data)
	for _, want := range []string{"recovered tool panic", "session=ses_123", "tool=apply_patch", "value=boom", "goroutine 1 [running]"} {
		if !strings.Contains(record, want) {
			t.Fatalf("diagnostics record missing %q, got: %s", want, record)
		}
	}
}
