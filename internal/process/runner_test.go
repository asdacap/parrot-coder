package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type memoryOutputStore struct{ data []byte }

type directSandbox struct{}

func (directSandbox) command(shell, script, _ string, _ []string) (string, []string, error) {
	return shell, []string{"-c", script}, nil
}

type recordingSandbox struct{ writable []string }

func (s *recordingSandbox) command(shell, script, _ string, writable []string) (string, []string, error) {
	s.writable = append([]string(nil), writable...)
	return shell, []string{"-c", script}, nil
}

func TestRunnerWritablePathsAreExactAndSessionScoped(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &recordingSandbox{}
	runner, err := NewRunner(Config{Workspace: ws, MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	runner.sandbox = sandbox
	if err := runner.AllowWrite("session-a", second); err != nil {
		t.Fatal(err)
	}
	if err := runner.AllowWrite("session-a", first); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(second, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runner.AllowWrite("session-a", child); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "true", Cwd: root, SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sandbox.writable, []string{first, second}) {
		t.Fatalf("session-a writable paths = %q", sandbox.writable)
	}
	if _, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "true", Cwd: root, SessionID: "session-b"}); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.writable) != 0 {
		t.Fatalf("session-b inherited writable paths: %q", sandbox.writable)
	}
}

func TestRunnerRejectsWritablePathChangedAfterApproval(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	granted := filepath.Join(external, "granted")
	other := filepath.Join(external, "other")
	for _, path := range []string{granted, other} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner, err := NewRunner(Config{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	runner.sandbox = directSandbox{}
	if err := runner.AllowWrite("session", granted); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(granted); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, granted); err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "true", SessionID: "session"})
	if err == nil || !strings.Contains(err.Error(), "writable path changed after approval") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunUnrestrictedBypassesSandbox(t *testing.T) {
	runner := testRunner(t, Config{})
	runner.sandbox = unsupportedSandbox{platform: "test"}
	result, err := runner.RunUnrestricted(context.Background(), Request{Shell: "/bin/sh", Command: "printf unrestricted"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "unrestricted" {
		t.Fatalf("output = %q", result.Output)
	}
}

func (s *memoryOutputStore) Store(_ context.Context, reader io.Reader) (StoredOutput, error) {
	data, err := io.ReadAll(reader)
	s.data = append([]byte(nil), data...)
	return StoredOutput{ID: "stored", Size: int64(len(data))}, err
}

func testRunner(t *testing.T, config Config) *Runner {
	t.Helper()
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	config.Workspace = ws
	config.sandbox = directSandbox{}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Second
	}
	runner, err := NewRunner(config)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestExitOutputCwdAndEnvironment(t *testing.T) {
	t.Setenv("LD_PRELOAD", "danger")
	t.Setenv("PYTHONSTARTUP", "danger")
	runner := testRunner(t, Config{Environment: map[string]string{"SAFE": "yes", "LD_PRELOAD": "ignored"}, MaxOutputBytes: 8})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf '%s' "$SAFE:$LD_PRELOAD:$PYTHONSTARTUP"; printf errx >&2; exit 7`})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || !result.Truncated || result.Output != "yes::err" {
		t.Fatalf("result = %#v", result)
	}
	if _, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "true", Env: map[string]string{"BASH_ENV": "x"}}); err == nil {
		t.Fatal("unsafe override accepted")
	}
	if _, err := runner.Run(context.Background(), Request{Shell: "sh", Command: "true"}); err == nil {
		t.Fatal("relative shell accepted")
	}
}

func TestRunDetectsShellWhenOmitted(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	runner := testRunner(t, Config{})
	result, err := runner.Run(context.Background(), Request{Command: "printf detected"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "detected" || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestStreamsFullOutputToStoreWhileBoundingMemory(t *testing.T) {
	store := &memoryOutputStore{}
	var streamed bytes.Buffer
	runner := testRunner(t, Config{MaxOutputBytes: 4, OutputStore: store})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf 12345; printf 67890 >&2`, Output: &streamed})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "1234" || !result.Truncated || result.OutputID != "stored" || result.OutputSize != 10 {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Equal(store.data, []byte("1234567890")) {
		t.Fatalf("stored output = %q", store.data)
	}
	if streamed.String() != "1234567890" {
		t.Fatalf("streamed output = %q", streamed.String())
	}
}

func TestOutputTailKeepsLastThreeLinesAndCarriageReturnReplacement(t *testing.T) {
	runner := testRunner(t, Config{MaxOutputBytes: 4})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf 'one\ntwo\n1%%\r2%%\rthree\nfour'`})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "one\n" || !result.Truncated || result.OutputTail != "two\nthree\nfour" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOutputTailPreservesUTF8AcrossWrites(t *testing.T) {
	var tail lineTail
	value := []byte("one\ntwo\n世界")
	tail.Write(value[:len(value)-1])
	tail.Write(value[len(value)-1:])
	if got := tail.String(); got != string(value) {
		t.Fatalf("tail = %q", got)
	}
}

func TestTimeoutAndCancellationCleanDescendants(t *testing.T) {
	runner := testRunner(t, Config{Timeout: 150 * time.Millisecond, TerminationGrace: 50 * time.Millisecond})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `trap '' TERM; while :; do sleep 1; done`})
	if err != nil || !result.TimedOut || result.Cancelled {
		t.Fatalf("timeout = %#v, %v", result, err)
	}

	root := runner.config.Workspace.Root()
	pidPath := filepath.Join(root, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := runner.Run(ctx, Request{Shell: "/bin/sh", Command: `trap '' TERM; sh -c 'trap "" TERM; while :; do sleep 1; done' & echo $! > child.pid; wait`})
		done <- struct {
			result Result
			err    error
		}{result, err}
	}()
	var pid int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("descendant pid not written")
	}
	cancel()
	outcome := <-done
	if outcome.err != nil || !outcome.result.Cancelled || outcome.result.TimedOut {
		t.Fatalf("cancel = %#v, %v", outcome.result, outcome.err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant survived cancellation")
}

func TestStartFailureClosesManagedOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{done: make(chan struct{})}
	runner, err := NewRunner(Config{Workspace: ws, OutputStore: store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), Request{Shell: filepath.Join(root, "missing-shell"), Command: "true"})
	if err == nil {
		t.Fatal("missing executable unexpectedly started")
	}
	select {
	case <-store.done:
	case <-time.After(time.Second):
		t.Fatal("managed output reader was not closed after start failure")
	}
}

type recordingStore struct {
	done chan struct{}
}

func (s *recordingStore) Store(_ context.Context, reader io.Reader) (StoredOutput, error) {
	_, err := io.Copy(io.Discard, reader)
	close(s.done)
	return StoredOutput{}, err
}
