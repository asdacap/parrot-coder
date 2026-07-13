package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type memoryOutputStore struct{ data []byte }

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

func TestStreamsFullOutputToStoreWhileBoundingMemory(t *testing.T) {
	store := &memoryOutputStore{}
	runner := testRunner(t, Config{MaxOutputBytes: 4, OutputStore: store})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf 12345; printf 67890 >&2`})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "1234" || !result.Truncated || result.OutputID != "stored" || result.OutputSize != 10 {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Equal(store.data, []byte("1234567890")) {
		t.Fatalf("stored output = %q", store.data)
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
