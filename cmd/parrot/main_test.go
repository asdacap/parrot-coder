package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestBuiltBinaryHelpAndVersionAreTerminalSafe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "parrot")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, argument := range []string{"help", "version"} {
		command := exec.CommandContext(ctx, binary, argument)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("parrot %s: %v\n%s", argument, err, output)
		}
		if len(bytes.TrimSpace(output)) == 0 {
			t.Fatalf("parrot %s returned empty output", argument)
		}
		if strings.ContainsRune(string(output), '\x1b') || strings.IndexFunc(string(output), func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\t'
		}) >= 0 {
			t.Fatalf("parrot %s emitted terminal controls: %q", argument, output)
		}
	}
}
