package diagnostics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLifecyclePanicAndStaleMarkerLogging(t *testing.T) {
	state := t.TempDir()
	runs := filepath.Join(state, directoryName, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := marker{RunID: "previous", PID: 99_999_999, StartedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
	if err := writeMarker(filepath.Join(runs, "previous.json"), previous); err != nil {
		t.Fatal(err)
	}

	run, err := Start(state, Build{Version: "test-version", Commit: "test-commit", Date: "test-date"})
	if err != nil {
		t.Fatal(err)
	}
	Event("test_event", "answer", 42)
	Warn("test_warning", "retry", true)
	Error("test_error", "error_type", ErrorType(errors.New("private error text")))
	panicForTest("where-test")
	run.Finish(7, "test_failure", "*errors.errorString")

	records := readRecords(t, filepath.Join(state, directoryName, logName))
	for _, expected := range []string{"unclean_previous_exit", "process_started", "test_event", "test_warning", "test_error", "panic_recovered", "process_exited"} {
		if findRecord(records, expected) == nil {
			t.Fatalf("missing event %q in %#v", expected, records)
		}
	}
	started := findRecord(records, "process_started")
	if started["version"] != "test-version" || started["commit"] != "test-commit" {
		t.Fatalf("process_started = %#v", started)
	}
	if warning := findRecord(records, "test_warning"); warning["level"] != "warn" {
		t.Fatalf("test_warning = %#v", warning)
	}
	if loggedError := findRecord(records, "test_error"); loggedError["level"] != "error" || strings.Contains(stringValue(loggedError["error_type"]), "private error text") {
		t.Fatalf("test_error = %#v", loggedError)
	}
	panicRecord := findRecord(records, "panic_recovered")
	if panicRecord["component"] != "where-test" || !strings.Contains(stringValue(panicRecord["stack"]), "panicForTest") {
		t.Fatalf("panic record does not identify its source: %#v", panicRecord)
	}
	exited := findRecord(records, "process_exited")
	if exited["exit_code"] != float64(7) || exited["exit_reason"] != "test_failure" || exited["error_type"] != "*errors.errorString" || exited["level"] != "error" {
		t.Fatalf("process_exited = %#v", exited)
	}
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("run markers remain after orderly exit: %#v", entries)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(state, directoryName), 0o700},
		{runs, 0o700},
		{filepath.Join(state, directoryName, logName), 0o600},
		{filepath.Join(state, directoryName, crashName), 0o600},
	} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != check.mode {
			t.Errorf("mode of %s = %o, want %o", check.path, info.Mode().Perm(), check.mode)
		}
	}
}

func TestFinishLevelReflectsFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		exitCode int
		level    string
	}{
		{name: "success", exitCode: 0, level: "info"},
		{name: "error", exitCode: 1, level: "error"},
		{name: "usage", exitCode: 2, level: "error"},
		{name: "interrupt", exitCode: 130, level: "info"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := t.TempDir()
			run, err := Start(state, Build{Version: "test-version"})
			if err != nil {
				t.Fatal(err)
			}
			run.Finish(test.exitCode, "some_reason")
			exited := findRecord(readRecords(t, filepath.Join(state, directoryName, logName)), "process_exited")
			if exited["exit_code"] != float64(test.exitCode) || exited["level"] != test.level {
				t.Fatalf("process_exited = %#v, want level %q", exited, test.level)
			}
		})
	}
}

func TestErrorTypeDoesNotIncludeErrorText(t *testing.T) {
	const secret = "secret-provider-response"
	if got := ErrorType(errors.New(secret)); got == "" || strings.Contains(got, secret) {
		t.Fatalf("ErrorType = %q", got)
	}
	if got := ErrorType(context.Canceled); got != "context_canceled" {
		t.Fatalf("canceled ErrorType = %q", got)
	}
	if got := ErrorType(context.DeadlineExceeded); got != "context_deadline_exceeded" {
		t.Fatalf("deadline ErrorType = %q", got)
	}
}

func TestRuntimeCrashOutput(t *testing.T) {
	if os.Getenv("PARROT_DIAGNOSTICS_CRASH_HELPER") == "1" {
		run, err := Start(os.Getenv("PARROT_DIAGNOSTICS_STATE"), Build{Version: "helper"})
		if err != nil {
			panic(err)
		}
		_ = run
		go func() { panic("diagnostic crash canary") }()
		select {}
	}

	state := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestRuntimeCrashOutput$")
	command.Env = append(os.Environ(), "PARROT_DIAGNOSTICS_CRASH_HELPER=1", "PARROT_DIAGNOSTICS_STATE="+state)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("crashing helper succeeded: %s", output)
	}
	data, err := os.ReadFile(filepath.Join(state, directoryName, crashName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "diagnostic crash canary") || !strings.Contains(text, "TestRuntimeCrashOutput") {
		t.Fatalf("crash log does not identify panic and location:\n%s", text)
	}
}

//go:noinline
func panicForTest(component string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			Panic(component, recovered)
		}
	}()
	panic("panic canary")
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode log line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func findRecord(records []map[string]any, event string) map[string]any {
	for _, record := range records {
		if record["event"] == event {
			return record
		}
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
