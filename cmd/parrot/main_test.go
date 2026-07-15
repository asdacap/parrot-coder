package main

import (
	"bytes"
	"context"
	"os"
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
	stateHome := filepath.Join(t.TempDir(), "state")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, argument := range []string{"help", "version"} {
		command := exec.CommandContext(ctx, binary, argument)
		command.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
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
	logData, err := os.ReadFile(filepath.Join(stateHome, "parrot", "diagnostics", "parrot.jsonl"))
	if err != nil {
		t.Fatalf("read diagnostics log: %v", err)
	}
	for _, event := range []string{`"event":"process_started"`, `"event":"command_started"`, `"event":"process_exited"`} {
		if !bytes.Contains(logData, []byte(event)) {
			t.Fatalf("diagnostics log does not contain %s: %s", event, logData)
		}
	}
	runs, err := os.ReadDir(filepath.Join(stateHome, "parrot", "diagnostics", "runs"))
	if err != nil {
		t.Fatalf("read run markers: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("orderly commands left run markers: %#v", runs)
	}
}
