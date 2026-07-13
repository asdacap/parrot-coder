package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run() code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "run        execute one prompt") {
		t.Fatalf("help output does not contain run command: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	app := New(BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-07-13"})
	code := app.Run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run() code = %d, want %d", code, exitOK)
	}
	for _, value := range []string{"1.2.3", "abc123", "2026-07-13"} {
		if !strings.Contains(stdout.String(), value) {
			t.Errorf("version output %q does not contain %q", stdout.String(), value)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"missing"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("Run() code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown command "missing"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
