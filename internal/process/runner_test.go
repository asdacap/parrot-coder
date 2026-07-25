package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type memoryOutputStore struct {
	data []byte
	omit int64
}

type memoryManagedOutput struct {
	store *memoryOutputStore
	data  []byte
}

func (o *memoryManagedOutput) Write(p []byte) (int, error) {
	o.data = append(o.data, p...)
	return len(p), nil
}

func (o *memoryManagedOutput) ID() string { return "stored" }

func (o *memoryManagedOutput) Finalize(context.Context) (StoredOutput, error) {
	data := o.data
	if o.store.omit > 0 && int64(len(data)) > o.store.omit {
		data = data[o.store.omit:]
	}
	o.store.data = append([]byte(nil), data...)
	return StoredOutput{ID: "stored", Size: int64(len(data)), OmittedBytes: o.store.omit}, nil
}

func (o *memoryManagedOutput) Discard() {}

type directSandbox struct{}

func (directSandbox) command(shell, script, _ string, _ security.SecurityProfile, _ string) (string, []string, error) {
	return shell, []string{"-c", script}, nil
}

func (directSandbox) temporaryDirectory(path string) string { return path }

type recordingSandbox struct {
	writable     []string
	temporaryDir string
}

func (s *recordingSandbox) command(shell, script, _ string, profile security.SecurityProfile, temporaryDirectory string) (string, []string, error) {
	s.writable = s.writable[:0]
	for _, rule := range profile.Rules() {
		if rule.Action == security.ActionAllowWrite {
			s.writable = append(s.writable, rule.Path)
		}
	}
	s.temporaryDir = temporaryDirectory
	return shell, []string{"-c", script}, nil
}

func (*recordingSandbox) temporaryDirectory(path string) string { return path }

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
	runner, err := NewRunner(Config{Workspace: ws, MaxOutputBytes: 1024, OutputStore: &memoryOutputStore{}})
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
	// session-a writable paths should include first, second, and workspace root
	if !slices.Contains(sandbox.writable, first) || !slices.Contains(sandbox.writable, second) {
		t.Fatalf("session-a writable paths = %q, want to contain %q and %q", sandbox.writable, first, second)
	}
	if _, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "true", Cwd: root, SessionID: "session-b"}); err != nil {
		t.Fatal(err)
	}
	// session-b writable paths should only have workspace root (no granted paths)
	if len(sandbox.writable) != 1 || sandbox.writable[0] != root {
		t.Fatalf("session-b writable paths = %q, want [%q]", sandbox.writable, root)
	}
}

func TestRunnerTemporaryDirectoriesAreSharedWithinSessionAndCleanedUp(t *testing.T) {
	temporaryRoot := t.TempDir()
	temporaryAlias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(temporaryRoot, temporaryAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporaryAlias)
	runner := testRunner(t, Config{})
	for _, request := range []Request{
		{Shell: "/bin/sh", Command: `printf shared > "$TMPDIR/marker"`, SessionID: "session-a"},
		{Shell: "/bin/sh", Command: `test "$(cat "$TMPDIR/marker")" = shared`, SessionID: "session-a"},
		{Shell: "/bin/sh", Command: `test ! -e "$TMPDIR/marker"`, SessionID: "session-b"},
	} {
		result, err := runner.Run(context.Background(), request)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("Run(%q) = %#v, %v", request.SessionID, result, err)
		}
	}
	first, second := runner.temporaryDirs["session-a"].path, runner.temporaryDirs["session-b"].path
	if first == "" || second == "" || first == second {
		t.Fatalf("temporary directories = %q, %q", first, second)
	}
	if !isPathWithin(first, temporaryRoot) || !isPathWithin(second, temporaryRoot) {
		t.Fatalf("temporary directories were not canonicalized: %q, %q", first, second)
	}
	if err := runner.DeleteSession("session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted session temporary directory stat error = %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed runner temporary directory stat error = %v", err)
	}
}

func TestRunnerTemporaryDirectoryCleanupHandlesRestrictedContentsAndClosedRunner(t *testing.T) {
	runner := testRunner(t, Config{})
	result, err := runner.Run(context.Background(), Request{
		Shell: "/bin/sh", Command: `mkdir "$TMPDIR/locked"; touch "$TMPDIR/locked/file"; chmod 000 "$TMPDIR/locked"`, SessionID: "session",
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	path := runner.temporaryDirs["session"].path
	if err := runner.DeleteSession("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary directory stat error = %v", err)
	}
	if _, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "true", SessionID: "session"}); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("persistent run after deletion error = %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "true", SessionID: "new"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("persistent run after close error = %v", err)
	}
	if len(runner.temporaryDirs) != 0 {
		t.Fatalf("temporary directories after rejected run = %v", runner.temporaryDirs)
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
	runner, err := NewRunner(Config{Workspace: ws, OutputStore: &memoryOutputStore{}})
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
	external := t.TempDir()
	external, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunUnrestricted(context.Background(), Request{Shell: "/bin/sh", Command: "pwd", Cwd: external})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Output) != external {
		t.Fatalf("output = %q", result.Output)
	}
}

func TestRunSandboxAllowsExternalWorkingDirectory(t *testing.T) {
	runner := testRunner(t, Config{})
	sandbox := &recordingSandbox{}
	runner.sandbox = sandbox
	external := t.TempDir()
	external, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "pwd", Cwd: external})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Output) != external {
		t.Fatalf("output = %q", result.Output)
	}
}

func (s *memoryOutputStore) Create(context.Context) (ManagedOutput, error) {
	return &memoryManagedOutput{store: s}, nil
}

func (s *memoryOutputStore) Read(_ string, offset, limit int64) ([]byte, error) {
	if offset < 0 || limit < 0 || offset > int64(len(s.data)) {
		return nil, errors.New("invalid output read")
	}
	return append([]byte(nil), s.data[offset:min(offset+limit, int64(len(s.data)))]...), nil
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
	if config.OutputStore == nil {
		config.OutputStore = &memoryOutputStore{}
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
	if result.ExitCode != 7 || !result.Truncated || result.Output != "yes:\n... 1 bytes omitted ...\nerrx" {
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

func TestStreamsFullOutputToStoreAndBoundsModelOutput(t *testing.T) {
	store := &memoryOutputStore{}
	var streamed bytes.Buffer
	runner := testRunner(t, Config{MaxOutputBytes: 4, OutputStore: store})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf 12345; printf 67890 >&2`, Output: &streamed})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "12\n... 6 bytes omitted ...\n90" || !result.Truncated || result.OutputID != "stored" || result.OutputSize != 10 {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Equal(store.data, []byte("1234567890")) {
		t.Fatalf("stored output = %q", store.data)
	}
	if streamed.String() != "1234567890" {
		t.Fatalf("streamed output = %q", streamed.String())
	}
}

func TestRunRequiresOutputStore(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(Config{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	runner.sandbox = directSandbox{}
	if _, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: "true"}); err == nil || !strings.Contains(err.Error(), "output store is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunReportsBytesLostFromStartOfStoredOutput(t *testing.T) {
	store := &memoryOutputStore{omit: 4}
	runner := testRunner(t, Config{OutputStore: store})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf 0123456789`})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "... first 4 bytes of output were lost ...\n456789" || !result.Truncated || result.OutputSize != 6 {
		t.Fatalf("result = %#v", result)
	}
}

func TestOutputTailKeepsLastTenLinesAndCarriageReturnReplacement(t *testing.T) {
	runner := testRunner(t, Config{MaxOutputBytes: 4})
	result, err := runner.Run(context.Background(), Request{Shell: "/bin/sh", Command: `printf 'one\ntwo\n1%%\r2%%\rthree\nfour'`})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "on\n... 20 bytes omitted ...\nur" || !result.Truncated || result.OutputTail != "one\ntwo\nthree\nfour" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOutputTailKeepsLastTenLinesAndPreservesUTF8AcrossWrites(t *testing.T) {
	var tail lineTail
	value := []byte("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n世界")
	tail.Write(value[:len(value)-1])
	tail.Write(value[len(value)-1:])
	want := "three\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n世界"
	if got := tail.String(); got != want {
		t.Fatalf("tail = %q, want %q", got, want)
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

func TestPersistentProcessPTYInputPollingOwnershipAndCleanup(t *testing.T) {
	runner := testRunner(t, Config{TerminationGrace: 50 * time.Millisecond})
	result, err := runner.RunPersistent(context.Background(), PersistentRequest{
		Shell: "/bin/sh", Command: `printf ready; read value; printf ':got:%s' "$value"`,
		SessionID: "owner", TTY: true, Yield: MinYieldTime,
	})
	if err != nil || result.ProcessID == nil || !strings.Contains(result.Output, "ready") {
		t.Fatalf("start = %#v, %v", result, err)
	}
	id := *result.ProcessID
	if _, err := runner.WritePersistent(context.Background(), PersistentWriteRequest{SessionID: "other", ProcessID: id, Chars: "secret\n", Yield: MinYieldTime}); err == nil || !strings.Contains(err.Error(), "unknown process") {
		t.Fatalf("cross-owner write error = %v", err)
	}
	result, err = runner.WritePersistent(context.Background(), PersistentWriteRequest{SessionID: "owner", ProcessID: id, Chars: "hello\n", Yield: time.Second})
	if err != nil || result.ProcessID != nil || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(result.Output, ":got:hello") {
		t.Fatalf("write = %#v, %v", result, err)
	}
	if _, err := runner.WritePersistent(context.Background(), PersistentWriteRequest{SessionID: "owner", ProcessID: id}); err == nil || !strings.Contains(err.Error(), "unknown process") {
		t.Fatalf("completed process retained: %v", err)
	}

	pipe, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: `sleep 5`, SessionID: "owner", Yield: MinYieldTime})
	if err != nil || pipe.ProcessID == nil {
		t.Fatalf("pipe start = %#v, %v", pipe, err)
	}
	if _, err := runner.WritePersistent(context.Background(), PersistentWriteRequest{SessionID: "owner", ProcessID: *pipe.ProcessID, Chars: "x", Yield: MinYieldTime}); err == nil || !strings.Contains(err.Error(), "stdin is closed") {
		t.Fatalf("pipe write error = %v", err)
	}
	if err := runner.InterruptSession("owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.WritePersistent(context.Background(), PersistentWriteRequest{SessionID: "owner", ProcessID: *pipe.ProcessID}); err == nil {
		t.Fatal("interrupted process retained")
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "true", SessionID: "owner", Yield: MinYieldTime}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("run after close = %v", err)
	}
}

func TestPersistentObserverWaitsWithoutConsumingOrControllingProcess(t *testing.T) {
	runner := testRunner(t, Config{TerminationGrace: 50 * time.Millisecond})
	result, err := runner.RunPersistent(context.Background(), PersistentRequest{
		Shell: "/bin/sh", Command: `sleep .35; printf final; exit 7`, SessionID: "owner", Yield: MinYieldTime,
	})
	if err != nil || result.ProcessID == nil {
		t.Fatalf("start = %#v, %v", result, err)
	}
	processID := *result.ProcessID
	observer, err := runner.ObservePersistent("owner", processID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runner.ObservePersistent("owner", processID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ObservePersistent("other", processID); err == nil || !strings.Contains(err.Error(), "unknown process") {
		t.Fatalf("cross-owner observation error = %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := observer.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled wait error = %v", err)
	}
	completion, err := observer.Wait(context.Background())
	if err != nil || completion.ProcessID != processID || completion.ExitCode == nil || *completion.ExitCode != 7 || completion.WaitError == nil {
		t.Fatalf("completion = %#v, %v", completion, err)
	}
	if active := runner.ListActivePersistent("owner"); len(active) != 0 {
		t.Fatalf("completed process listed as active: %#v", active)
	}

	drained, err := runner.WritePersistent(context.Background(), PersistentWriteRequest{
		SessionID: "owner", ProcessID: processID, Chars: "\x03", Yield: MinYieldTime,
	})
	if err != nil || drained.ProcessID != nil || drained.ExitCode == nil || *drained.ExitCode != 7 || drained.Output != "final" {
		t.Fatalf("drain = %#v, %v", drained, err)
	}
	completion, err = second.Wait(context.Background())
	if err != nil || completion.ExitCode == nil || *completion.ExitCode != 7 {
		t.Fatalf("stable observer completion = %#v, %v", completion, err)
	}
	if _, err := (*PersistentObserver)(nil).Wait(context.Background()); err == nil {
		t.Fatal("nil observer wait succeeded")
	}
}

func TestPersistentIDsActiveListingAndTargetedInterruptionAreOwnerScoped(t *testing.T) {
	runner := testRunner(t, Config{TerminationGrace: 50 * time.Millisecond})
	t.Cleanup(func() { _ = runner.Close() })
	startedAfter := time.Now()
	results := make([]PersistentResult, 0, 3)
	for _, owner := range []string{"owner", "owner", "other"} {
		result, err := runner.RunPersistent(context.Background(), PersistentRequest{
			Shell: "/bin/sh", Command: "sleep 5", SessionID: owner, Yield: MinYieldTime,
		})
		if err != nil || result.ProcessID == nil {
			t.Fatalf("start for %q = %#v, %v", owner, result, err)
		}
		if err := id.ValidatePrefix(*result.ProcessID, "proc"); err != nil {
			t.Fatalf("process ID %q: %v", *result.ProcessID, err)
		}
		results = append(results, result)
	}

	owned := runner.ListActivePersistent("owner")
	other := runner.ListActivePersistent("other")
	if len(owned) != 2 || owned[0].ID != *results[0].ProcessID || owned[1].ID != *results[1].ProcessID ||
		owned[0].SessionID != "owner" || owned[1].SessionID != "owner" ||
		owned[0].StartedAt.Before(startedAfter) || owned[1].StartedAt.Before(owned[0].StartedAt) ||
		len(other) != 1 || other[0].ID != *results[2].ProcessID || other[0].SessionID != "other" || len(runner.ListActivePersistent("unknown")) != 0 {
		t.Fatalf("active tasks: owner=%#v other=%#v", owned, other)
	}
	if _, err := runner.InterruptPersistent("other", owned[0].ID); err == nil || !strings.Contains(err.Error(), "unknown process") {
		t.Fatalf("cross-owner interrupt error = %v", err)
	}
	if active := runner.ListActivePersistent("owner"); len(active) != 2 {
		t.Fatalf("cross-owner interrupt changed active tasks: %#v", active)
	}
	interrupted, err := runner.InterruptPersistent("owner", owned[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted != owned[0] {
		t.Fatalf("interrupted task = %#v, want %#v", interrupted, owned[0])
	}
	if active := runner.ListActivePersistent("owner"); len(active) != 1 || active[0].ID != owned[1].ID {
		t.Fatalf("active tasks after targeted interrupt = %#v", active)
	}
	if active := runner.ListActivePersistent("other"); len(active) != 1 || active[0].ID != other[0].ID {
		t.Fatalf("targeted interrupt affected other owner: %#v", active)
	}
	if _, err := runner.InterruptPersistent("owner", owned[0].ID); err == nil || !strings.Contains(err.Error(), "unknown process") {
		t.Fatalf("repeated interrupt error = %v", err)
	}
}

func TestPersistentProcessLimitsReapCompletedAndBoundConcurrentReservations(t *testing.T) {
	runner := testRunner(t, Config{MaxProcesses: 2, MaxSessionProcesses: 1, TerminationGrace: 50 * time.Millisecond})
	first, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "sleep .35", SessionID: "a", Yield: MinYieldTime})
	if err != nil || first.ProcessID == nil {
		t.Fatalf("first = %#v, %v", first, err)
	}
	if _, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "sleep 1", SessionID: "a", Yield: MinYieldTime}); err == nil || !strings.Contains(err.Error(), "session process limit") {
		t.Fatalf("session limit error = %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	second, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "sleep 1", SessionID: "a", Yield: MinYieldTime})
	if err != nil || second.ProcessID == nil {
		t.Fatalf("completed process was not reaped: %#v, %v", second, err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}

	limited := testRunner(t, Config{MaxProcesses: 1, MaxSessionProcesses: 2})
	limited.mu.Lock()
	limited.reservedIDs["proc_00000000000000000000000000"] = "a"
	limited.mu.Unlock()
	if _, _, err := limited.reserveProcessID("b"); err == nil || !strings.Contains(err.Error(), "global process limit") {
		t.Fatalf("global reservation limit error = %v", err)
	}
}

func TestPersistentProcessRejectsCrossSessionEvictionAndCleansExitedGroups(t *testing.T) {
	runner := testRunner(t, Config{MaxProcesses: 1, MaxSessionProcesses: 1, TerminationGrace: 50 * time.Millisecond})
	first, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "sleep 5", SessionID: "a", Yield: MinYieldTime})
	if err != nil || first.ProcessID == nil {
		t.Fatalf("first = %#v, %v", first, err)
	}
	if _, err := runner.RunPersistent(context.Background(), PersistentRequest{Shell: "/bin/sh", Command: "sleep 5", SessionID: "b", Yield: MinYieldTime}); err == nil || !strings.Contains(err.Error(), "global process limit") {
		t.Fatalf("cross-session limit error = %v", err)
	}
	if _, err := runner.WritePersistent(context.Background(), PersistentWriteRequest{SessionID: "a", ProcessID: *first.ProcessID, Chars: "\x03", Yield: time.Second}); err != nil {
		t.Fatalf("first process was evicted: %v", err)
	}

	result, err := runner.RunPersistent(context.Background(), PersistentRequest{
		Shell: "/bin/sh", Command: `sleep 5 & printf %d $!`, SessionID: "a", Yield: time.Second,
	})
	if err != nil || result.ProcessID != nil || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("detached child result = %#v, %v", result, err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(result.Output))
	if err != nil {
		t.Fatalf("child pid %q: %v", result.Output, err)
	}
	deadline := time.Now().Add(time.Second)
	for processExists(child) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(child) {
		t.Fatalf("descendant %d survived shell exit", child)
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestPersistentOutputFormattingPreservesHeadTailAndUTF8(t *testing.T) {
	buffer := newHeadTailBuffer(8)
	buffer.push([]byte("ab世界cd"))
	output := buffer.text()
	if !utf8.ValidString(output) || !strings.Contains(output, "bytes omitted") {
		t.Fatalf("output = %q", output)
	}
	tokens := 1
	truncated := truncatePersistentOutput("abcdefgh", &tokens, 2, 0)
	if !strings.Contains(truncated, "Warning: truncated output") || !strings.Contains(truncated, "tokens truncated") {
		t.Fatalf("truncated = %q", truncated)
	}
}

type recordingStore struct {
	done chan struct{}
}

type recordingManagedOutput struct {
	store *recordingStore
	once  sync.Once
}

func (o *recordingManagedOutput) Write(p []byte) (int, error) { return len(p), nil }
func (o *recordingManagedOutput) ID() string                  { return "recording" }
func (o *recordingManagedOutput) Finalize(context.Context) (StoredOutput, error) {
	o.once.Do(func() { close(o.store.done) })
	return StoredOutput{ID: "recording"}, nil
}
func (o *recordingManagedOutput) Discard() { o.once.Do(func() { close(o.store.done) }) }

func TestPersistentEnvironmentOverridesHygieneDefaultsAndRejectsUnsafeNames(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		read    string
		want    string
		wantErr string
	}{
		{name: "hygiene default applies", read: "TERM", want: "dumb"},
		{name: "caller overrides hygiene", env: map[string]string{"TERM": "xterm"}, read: "TERM", want: "xterm"},
		{name: "caller value passes through", env: map[string]string{"PARROT_TEST": "value"}, read: "PARROT_TEST", want: "value"},
		{name: "unrelated caller value keeps hygiene", env: map[string]string{"PARROT_TEST": "value"}, read: "NO_COLOR", want: "1"},
		{name: "unsafe name rejected", env: map[string]string{"LD_PRELOAD": "/tmp/evil.so"}, read: "TERM", wantErr: "unsafe environment variable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := testRunner(t, Config{TerminationGrace: 50 * time.Millisecond})
			result, err := runner.RunPersistent(context.Background(), PersistentRequest{
				Shell: "/bin/sh", Command: `printf '%s' "$` + test.read + `"`,
				Env: test.env, SessionID: "owner", Yield: time.Second,
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunPersistent = %v", err)
			}
			if !strings.Contains(result.Output, test.want) {
				t.Fatalf("$%s output = %q, want containing %q", test.read, result.Output, test.want)
			}
		})
	}
}

func (s *recordingStore) Create(context.Context) (ManagedOutput, error) {
	return &recordingManagedOutput{store: s}, nil
}

func (s *recordingStore) Read(string, int64, int64) ([]byte, error) { return nil, nil }

type recordingRulesSandbox struct {
	rules []security.Rule
}

func (s *recordingRulesSandbox) command(_ string, _ string, _ string, profile security.SecurityProfile, _ string) (string, []string, error) {
	s.rules = append([]security.Rule(nil), profile.Rules()...)
	return "/bin/sh", []string{"-c", "true"}, nil
}

func (*recordingRulesSandbox) temporaryDirectory(path string) string { return path }

type testSecurityProfile struct {
	readOnly  bool
	writePath string
}

func (p testSecurityProfile) IsReadOnly() bool { return p.readOnly }
func (p testSecurityProfile) Rules() []security.Rule {
	return []security.Rule{{Path: p.writePath, Action: security.ActionAllowWrite}}
}

func TestBuildProfileProtectsReadOnlyWritesAndPreservesWritableRules(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(artifact, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rules := []security.Rule{
		{Path: "/", Action: security.ActionAllowWrite},
		{Path: artifact, Action: security.ActionDenyWrite},
		{Path: "/secret", Action: security.ActionDenyRead},
	}
	runner, err := NewRunner(Config{Workspace: ws, OutputStore: &memoryOutputStore{}, SandboxRules: rules})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		profile testSecurityProfile
		want    []security.Rule
	}{
		{
			name:    "read-only",
			profile: testSecurityProfile{readOnly: true, writePath: artifact},
			want: []security.Rule{
				{Path: artifact, Action: security.ActionAllowWrite},
				rules[2],
			},
		},
		{
			name:    "writable",
			profile: testSecurityProfile{writePath: artifact},
			want: append([]security.Rule{
				{Path: root, Action: security.ActionAllowWrite},
				{Path: artifact, Action: security.ActionAllowWrite},
			}, rules...),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, err := runner.buildProfile(test.profile, test.name, root)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(profile.Rules(), test.want) {
				t.Fatalf("Rules() = %#v, want %#v", profile.Rules(), test.want)
			}
		})
	}
}

func TestBuildProfileIncludesSandboxRules(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	configRules := []security.Rule{
		{Path: "/opt/cache", Action: security.ActionAllowWrite},
		{Path: "/secret", Action: security.ActionDenyRead},
	}
	sandbox := &recordingRulesSandbox{}
	runner, err := NewRunner(Config{
		Workspace:    ws,
		OutputStore:  &memoryOutputStore{},
		SandboxRules: configRules,
		sandbox:      sandbox,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), Request{
		Shell:     "/bin/sh",
		Command:   "true",
		SessionID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]security.Rule{{Path: root, Action: security.ActionAllowWrite}}, configRules...)
	if !slices.Equal(sandbox.rules, want) {
		t.Fatalf("rules = %#v, want %#v", sandbox.rules, want)
	}
}
