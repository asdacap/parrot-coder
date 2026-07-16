package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestStartupWarnsWhenExpectedCLIUtilitiesAreMissing(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "warning")
	if err != nil {
		t.Fatal(err)
	}
	warnMissingCLIUtilities(output, func(string) (string, error) { return "", errors.New("not found") })
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, utility := range []string{"bash", "bwrap", "git", "rg", "stat"} {
		if !bytes.Contains(data, []byte(utility)) {
			t.Fatalf("warning %q does not contain %q", data, utility)
		}
	}
}

func TestStartupDoesNotWarnWhenExpectedCLIUtilitiesAreAvailable(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "warning")
	if err != nil {
		t.Fatal(err)
	}
	warnMissingCLIUtilities(output, func(name string) (string, error) { return "/bin/" + name, nil })
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("unexpected warning %q", data)
	}
}

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
	for _, event := range []string{`"event":"process_started"`, `"event":"command_started"`, `"event":"command_finished"`, `"event":"process_exited"`, `"exit_reason":"help_displayed"`, `"exit_reason":"version_displayed"`} {
		if !bytes.Contains(logData, []byte(event)) {
			t.Fatalf("diagnostics log does not contain %s: %s", event, logData)
		}
	}
	for _, line := range bytes.Split(bytes.TrimSpace(logData), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode diagnostics record: %v", err)
		}
		if record["event"] == "command_finished" || record["event"] == "process_exited" {
			if reason, ok := record["exit_reason"].(string); !ok || reason == "" {
				t.Fatalf("exit record has no reason: %#v", record)
			}
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
