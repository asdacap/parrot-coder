package process

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/security"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/creack/pty"
	petname "github.com/dustinkirkland/golang-petname"
)

const (
	MinYieldTime          = 250 * time.Millisecond
	MinEmptyPollYieldTime = 5 * time.Second
	MaxYieldTime          = 30 * time.Second
	MaxEmptyPollYieldTime = 5 * time.Minute
	DefaultExecYieldTime  = 10 * time.Second
	DefaultWriteYieldTime = 250 * time.Millisecond
	DefaultOutputTokens   = 10_000
	MaxOutputTokens       = 262_144
	persistentOutputBytes = 1 << 20
)

type PersistentRequest struct {
	Shell           string
	Command         string
	Name            string
	Cwd             string
	Env             map[string]string
	SessionID       string
	Yield           time.Duration
	MaxOutputTokens *int
	TTY             bool
	Output          io.Writer
	Unrestricted    bool
	SecurityProfile security.SecurityProfile
}

type PersistentWriteRequest struct {
	SessionID       string
	ProcessID       string
	Chars           string
	Yield           time.Duration
	MaxOutputTokens *int
	Output          io.Writer
}

type PersistentResult struct {
	ChunkID            string
	Name               string
	WallTime           time.Duration
	Output             string
	ProcessID          *string
	ExitCode           *int
	OriginalTokenCount int
	OmittedBytes       int
	OutputPath         string
	OutputSize         int64
	Truncated          bool
}

// PersistentCompletion describes the terminal state of a managed process.
// WaitError is populated when the operating-system wait failed, including
// processes which exited unsuccessfully or because of a signal.
type PersistentCompletion struct {
	ProcessID string
	ExitCode  *int
	WaitError error
}

// PersistentTask identifies an active managed process, its owning session, and
// when it started.
type PersistentTask struct {
	ProcessID string
	Name      string
	SessionID string
	StartedAt time.Time
}

// PersistentObserver holds a stable reference to a managed process. It remains
// valid if another caller later drains the process output and removes the
// process from the runner's interactive-process registry.
type PersistentObserver struct{ process *persistentProcess }

// ObservePersistent validates ownership and returns a reusable completion
// observer. Multiple observers may safely wait for the same process.
func (r *Runner) ObservePersistent(sessionID, processID string) (*PersistentObserver, error) {
	item := r.lookupPersistent(sessionID, processID)
	if item == nil {
		return nil, fmt.Errorf("process: unknown process id %q", processID)
	}
	return &PersistentObserver{process: item}, nil
}

// Wait waits for the observed process without consuming its output or removing
// it from the runner. Canceling the context does not affect the process.
func (o *PersistentObserver) Wait(ctx context.Context) (PersistentCompletion, error) {
	if o == nil || o.process == nil {
		return PersistentCompletion{}, errors.New("process: observer is required")
	}
	select {
	case <-o.process.finished:
		o.process.mu.Lock()
		completion := PersistentCompletion{ProcessID: o.process.id, ExitCode: o.process.exitCode, WaitError: o.process.waitErr}
		o.process.mu.Unlock()
		return completion, nil
	case <-ctx.Done():
		return PersistentCompletion{}, ctx.Err()
	}
}

type persistentProcess struct {
	id           string
	name         string
	sessionID    string
	command      *exec.Cmd
	stdin        io.WriteCloser
	reader       io.ReadCloser
	tty          bool
	started      time.Time
	lastUsed     time.Time
	interaction  sync.Mutex
	terminate    sync.Once
	announced    bool
	finishedSent bool

	mu                  sync.Mutex
	output              headTailBuffer
	stream              io.Writer
	waitErr             error
	exitCode            *int
	largeOutput         Output
	storedOutput        *StoredOutput
	storeErr            error
	maxOutputTokens     *int
	completionDelivered bool
	notify              chan struct{}
	finished            chan struct{}
	readerDone          chan struct{}
}

func (r *Runner) emitPersistent(event PersistentEvent) {
	if handler := r.persistentEventHandler(); handler != nil {
		handler(event)
	}
}

// RunPersistent starts a command in the mandatory sandbox and returns after it
// exits or the requested yield period elapses. A running command is retained
// under the returned process ID for WritePersistent calls.
func (r *Runner) RunPersistent(ctx context.Context, request PersistentRequest) (PersistentResult, error) {
	if request.Shell == "" {
		var err error
		request.Shell, err = DefaultShell()
		if err != nil {
			return PersistentResult{}, err
		}
	}
	if request.SessionID == "" || !filepath.IsAbs(request.Shell) || request.Command == "" {
		return PersistentResult{}, errors.New("process: session, absolute shell path, and command are required")
	}
	if err := ctx.Err(); err != nil {
		return PersistentResult{}, err
	}
	resolved, err := ResolveWorkingDirectory(request.Cwd, r.config.WorkingDirectory)
	if err != nil {
		return PersistentResult{}, fmt.Errorf("process: resolve cwd: %w", err)
	}
	// Caller-supplied values are merged last so an explicit request overrides
	// the output-hygiene defaults rather than being silently dropped.
	overrides := map[string]string{
		"NO_COLOR": "1", "TERM": "dumb", "LANG": "C.UTF-8", "LC_CTYPE": "C.UTF-8", "LC_ALL": "C.UTF-8",
		"COLORTERM": "", "PAGER": "cat", "GIT_PAGER": "cat", "GH_PAGER": "cat", "CODEX_CI": "1",
	}
	for name, value := range request.Env {
		overrides[name] = value
	}
	environment, err := r.environment(overrides)
	if err != nil {
		return PersistentResult{}, err
	}
	resolvedShell, err := executableFile(request.Shell)
	if err != nil {
		return PersistentResult{}, fmt.Errorf("process: shell: %w", err)
	}
	program, arguments := resolvedShell, []string{"-c", request.Command}
	var releaseTemporaryDirectory func()
	if !request.Unrestricted {
		temporaryDirectory, release, temporaryErr := r.acquireTemporaryDirectory(request.SessionID)
		if temporaryErr != nil {
			return PersistentResult{}, temporaryErr
		}
		releaseTemporaryDirectory = release
		defer func() {
			if releaseTemporaryDirectory != nil {
				releaseTemporaryDirectory()
			}
		}()
		setEnvironment(environment, "TMPDIR", r.sandbox.temporaryDirectory(temporaryDirectory))
		profile, buildErr := r.buildProfile(request.SecurityProfile, request.SessionID, resolved)
		if buildErr != nil {
			return PersistentResult{}, buildErr
		}
		program, arguments, err = r.sandbox.command(resolvedShell, request.Command, resolved, profile, temporaryDirectory)
		if err != nil {
			return PersistentResult{}, fmt.Errorf("process: sandbox: %w", err)
		}
	}

	processID, processName, pruned, err := r.reservePersistent(request.SessionID, request.Name)
	if err != nil {
		return PersistentResult{}, err
	}
	if pruned != nil {
		r.terminatePersistent(pruned)
	}
	if r.config.OutputStore == nil {
		r.releaseReservedID(processID)
		return PersistentResult{}, errors.New("process: output store is required")
	}
	largeOutput, err := r.config.OutputStore.Create(ctx, request.SessionID)
	if err != nil {
		r.releaseReservedID(processID)
		return PersistentResult{}, fmt.Errorf("process: create output: %w", err)
	}
	command := exec.Command(program, arguments...)
	command.Dir, command.Env = resolved, environment
	item := &persistentProcess{
		id: processID, name: processName, sessionID: request.SessionID, command: command, tty: request.TTY,
		started: time.Now(), lastUsed: time.Now(), output: newHeadTailBuffer(persistentOutputBytes),
		largeOutput: largeOutput, maxOutputTokens: cloneInt(request.MaxOutputTokens),
		notify: make(chan struct{}, 1), finished: make(chan struct{}), readerDone: make(chan struct{}),
	}
	if err := r.startPersistent(ctx, item); err != nil {
		largeOutput.Discard()
		r.releaseReservedID(processID)
		return PersistentResult{}, fmt.Errorf("process: start: %w", err)
	}
	if !r.storePersistent(item) {
		r.terminatePersistent(item)
		return PersistentResult{}, errors.New("process: runner is closed")
	}
	if releaseTemporaryDirectory != nil {
		releaseTemporaryDirectory()
		releaseTemporaryDirectory = nil
	}
	result, err := r.collectPersistent(ctx, item, clampExecYield(request.Yield), request.MaxOutputTokens, request.Output)
	if err != nil {
		r.removePersistent(item)
		r.terminatePersistent(item)
		return PersistentResult{}, err
	}
	if result.ProcessID == nil {
		r.removePersistent(item)
	} else {
		if r.config.Tasks != nil {
			if err := r.config.Tasks.Register(&managedShellTask{runner: r, process: item}, func(caller string) bool { return caller == item.sessionID }); err != nil {
				r.removePersistent(item)
				r.terminatePersistent(item)
				return PersistentResult{}, err
			}
		}
		r.announcePersistent(item)
		r.notifyPersistentCompletion(item)
	}
	return result, nil
}

type activeNotification struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *Runner) notifyPersistentCompletion(item *persistentProcess) {
	sessions := r.agentSessions()
	if sessions == nil {
		diagnostics.Warn("shell_process_notification_unavailable", "session_id", item.sessionID, "process_name", item.name)
		return
	}
	r.mu.Lock()
	if r.closed || r.notifyPaused[item.sessionID] > 0 {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(r.notifyCtx)
	notification := &activeNotification{cancel: cancel, done: make(chan struct{})}
	if r.notifications[item.sessionID] == nil {
		r.notifications[item.sessionID] = make(map[string]*activeNotification)
	}
	r.notifications[item.sessionID][item.id] = notification
	r.notifyWG.Add(1)
	r.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			r.mu.Lock()
			if r.notifications[item.sessionID][item.id] == notification {
				delete(r.notifications[item.sessionID], item.id)
				if len(r.notifications[item.sessionID]) == 0 {
					delete(r.notifications, item.sessionID)
				}
			}
			close(notification.done)
			r.mu.Unlock()
			r.notifyWG.Done()
		}()
		select {
		case <-item.finished:
		case <-ctx.Done():
			return
		}
		// A poll which was already waiting when the process finished owns delivery.
		// Serialize behind it so the automatic notification cannot send the same
		// terminal output concurrently.
		item.interaction.Lock()
		defer item.interaction.Unlock()
		item.mu.Lock()
		if item.completionDelivered {
			item.mu.Unlock()
			return
		}
		waitErr, maxOutputTokens := item.waitErr, item.maxOutputTokens
		item.mu.Unlock()

		result, err := r.persistentResult(item, 0, false, maxOutputTokens)
		if err != nil {
			diagnostics.Error("shell_process_output_failed", "session_id", item.sessionID, "process_name", item.name, "error_type", diagnostics.ErrorType(err))
			result = r.persistentResultFallback(item, 0, false)
		}
		content := result.completionNotification(waitErr)
		sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
		defer sendCancel()
		if sendCtx.Err() != nil {
			return
		}
		session := sessions.Get(item.sessionID)
		if session == nil {
			diagnostics.Warn("shell_process_notification_unavailable", "session_id", item.sessionID, "process_name", item.name)
			return
		}
		messageID, err := id.New("msg")
		if err == nil {
			_, err = session.Send(sendCtx, messageID, content)
		}
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				diagnostics.Error("shell_process_notification_failed", "session_id", item.sessionID, "process_name", item.name, "error_type", diagnostics.ErrorType(err))
			}
			return
		}
		item.mu.Lock()
		item.completionDelivered = true
		item.mu.Unlock()
	}()
}

// announcePersistent promotes a retained process to a shell task. A process
// which exits before promotion never emits task events; one which exits
// afterwards emits exactly one finished event, from whichever side observes
// the exit first.
func (r *Runner) announcePersistent(item *persistentProcess) {
	item.mu.Lock()
	item.announced = true
	started := item.started
	item.mu.Unlock()
	r.emitPersistent(PersistentEvent{Kind: PersistentEventStart, SessionID: item.sessionID, ProcessID: item.id, Name: item.name, StartedAt: started})
	select {
	case <-item.finished:
		item.mu.Lock()
		if item.finishedSent {
			item.mu.Unlock()
			return
		}
		item.finishedSent = true
		exitCode, waitErr := item.exitCode, item.waitErr
		item.mu.Unlock()
		r.emitPersistent(finishedPersistentEvent(item, exitCode, waitErr))
	default:
	}
}

func finishedPersistentEvent(item *persistentProcess, exitCode *int, waitErr error) PersistentEvent {
	event := PersistentEvent{Kind: PersistentEventFinished, SessionID: item.sessionID, ProcessID: item.id, Name: item.name, StartedAt: item.started, ExitCode: exitCode}
	if waitErr != nil {
		event.Error = waitErr.Error()
	}
	return event
}

// WritePersistent writes to or polls an existing process. Pipe-backed
// processes accept only Ctrl-C, matching Codex unified exec behavior.
func (r *Runner) WritePersistent(ctx context.Context, request PersistentWriteRequest) (PersistentResult, error) {
	item := r.lookupPersistent(request.SessionID, request.ProcessID)
	if item == nil {
		return PersistentResult{}, fmt.Errorf("process: unknown process id %q", request.ProcessID)
	}
	item.interaction.Lock()
	defer item.interaction.Unlock()
	if !r.ownsPersistent(item) {
		return PersistentResult{}, fmt.Errorf("process: unknown process id %q", request.ProcessID)
	}
	if request.Chars != "" {
		if !item.tty {
			if request.Chars != "\x03" {
				return PersistentResult{}, errors.New("process: stdin is closed")
			}
			_ = syscall.Kill(-item.command.Process.Pid, syscall.SIGINT)
		} else if _, err := io.WriteString(item.stdin, request.Chars); err != nil {
			select {
			case <-item.finished:
			default:
				return PersistentResult{}, fmt.Errorf("process: write stdin: %w", err)
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	yield := request.Yield
	if request.Chars == "" {
		if yield < MinEmptyPollYieldTime {
			yield = MinEmptyPollYieldTime
		}
		if yield > MaxEmptyPollYieldTime {
			yield = MaxEmptyPollYieldTime
		}
	} else {
		yield = clampExecYield(yield)
	}
	result, err := r.collectPersistent(ctx, item, yield, request.MaxOutputTokens, request.Output)
	if err != nil {
		return PersistentResult{}, err
	}
	if result.ProcessID == nil {
		item.mu.Lock()
		alreadyDelivered := item.completionDelivered
		item.completionDelivered = true
		item.mu.Unlock()
		if alreadyDelivered {
			result.Output, result.OriginalTokenCount, result.OmittedBytes = "", 0, 0
			result.Truncated = false
		}
		r.removePersistent(item)
	}
	return result, nil
}

func (r *Runner) startPersistent(ctx context.Context, item *persistentProcess) error {
	if item.tty {
		terminal, err := pty.Start(item.command)
		if err != nil {
			return err
		}
		item.stdin = terminal
		go func() {
			_, _ = io.Copy(persistentOutputWriter{process: item}, terminal)
			_ = terminal.Close()
			close(item.readerDone)
		}()
	} else {
		reader, writer, err := os.Pipe()
		if err != nil {
			return err
		}
		item.command.Stdin = nil
		item.command.Stdout = writer
		item.command.Stderr = writer
		item.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := item.command.Start(); err != nil {
			_ = reader.Close()
			_ = writer.Close()
			return err
		}
		_ = writer.Close()
		item.reader = reader
		go func() {
			_, _ = io.Copy(persistentOutputWriter{process: item}, reader)
			_ = reader.Close()
			close(item.readerDone)
		}()
	}
	go func() {
		err := item.command.Wait()
		r.terminateExitedGroup(item.command.Process.Pid)
		if item.tty {
			_ = item.stdin.Close()
		} else {
			_ = item.reader.Close()
		}
		<-item.readerDone
		stored, storeErr := item.largeOutput.Finalize(context.WithoutCancel(ctx))
		item.mu.Lock()
		item.waitErr = err
		if item.command.ProcessState != nil {
			code := item.command.ProcessState.ExitCode()
			item.exitCode = &code
		}
		item.storedOutput = &stored
		item.storeErr = storeErr
		emit := item.announced && !item.finishedSent
		if emit {
			item.finishedSent = true
		}
		exitCode, waitErr := item.exitCode, item.waitErr
		item.mu.Unlock()
		close(item.finished)
		item.signalOutput()
		if emit {
			r.emitPersistent(finishedPersistentEvent(item, exitCode, waitErr))
		}
	}()
	return nil
}

func (r *Runner) terminateExitedGroup(pid int) {
	if !groupExists(pid) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.NewTimer(r.config.TerminationGrace)
	defer deadline.Stop()
	for groupExists(pid) {
		select {
		case <-deadline.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type persistentOutputWriter struct{ process *persistentProcess }

func (w persistentOutputWriter) Write(value []byte) (int, error) {
	w.process.mu.Lock()
	w.process.output.push(value)
	if w.process.largeOutput != nil {
		_, _ = w.process.largeOutput.Write(value)
	}
	if w.process.stream != nil {
		_, _ = w.process.stream.Write(value)
	}
	w.process.mu.Unlock()
	w.process.signalOutput()
	return len(value), nil
}

func (p *persistentProcess) signalOutput() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func (r *Runner) collectPersistent(ctx context.Context, item *persistentProcess, yield time.Duration, maxTokens *int, stream io.Writer) (PersistentResult, error) {
	started := time.Now()
	item.mu.Lock()
	item.stream = stream
	item.lastUsed = started
	item.mu.Unlock()
	defer func() {
		item.mu.Lock()
		if item.stream == stream {
			item.stream = nil
		}
		item.mu.Unlock()
	}()
	timer := time.NewTimer(yield)
	defer timer.Stop()
	for {
		select {
		case <-item.finished:
			return r.persistentResult(item, time.Since(started), false, maxTokens)
		case <-timer.C:
			select {
			case <-item.finished:
				return r.persistentResult(item, time.Since(started), false, maxTokens)
			default:
				return r.persistentResult(item, time.Since(started), true, maxTokens)
			}
		case <-ctx.Done():
			return PersistentResult{}, ctx.Err()
		}
	}
}

func (result PersistentResult) completionNotification(waitErr error) string {
	content := fmt.Sprintf("Shell process notification: process %s finished.", result.Name)
	if result.ExitCode != nil {
		content = fmt.Sprintf("Shell process notification: process %s exited with code %d.", result.Name, *result.ExitCode)
	}
	if waitErr != nil {
		content += "\n\nError: " + waitErr.Error()
	}
	if result.Output != "" {
		if result.Truncated {
			content += "\n\nOutput (tail only; full output is too large):\n" + result.Output
		} else {
			content += "\n\nOutput:\n" + result.Output
		}
	}
	if result.Truncated && result.OutputPath != "" {
		content += "\n\nFull output is available at " + result.OutputPath + ". Read that file separately for the complete output."
	}
	return content
}

func (r *Runner) persistentResult(item *persistentProcess, wallTime time.Duration, running bool, maxTokens *int) (PersistentResult, error) {
	item.mu.Lock()
	output := item.output.drain()
	exitCode := item.exitCode
	var largeOutput Output
	if item.largeOutput != nil {
		largeOutput = item.largeOutput
	}
	storedOutput := item.storedOutput
	storeErr := item.storeErr
	item.mu.Unlock()

	outputPath := ""
	if largeOutput != nil {
		outputPath = largeOutput.Path()
	} else if storedOutput != nil {
		outputPath = storedOutput.Path
	}
	result := PersistentResult{
		ChunkID: generateChunkID(), Name: item.name, WallTime: wallTime,
		ExitCode: exitCode, OutputPath: outputPath,
	}

	if running {
		text := output.text()
		truncated, totalTokens, omittedTokens, err := r.tokenizer.TruncateMiddle(text, normalizeOutputTokens(maxTokens))
		if err != nil {
			return PersistentResult{}, fmt.Errorf("process: truncate running output: %w", err)
		}
		result.Output = formatPersistentOutput(text, truncated, totalTokens, omittedTokens, output.omitted)
		result.OriginalTokenCount = totalTokens
		result.OmittedBytes = output.omitted
		result.Truncated = output.omitted > 0 || omittedTokens > 0
		id := item.id
		result.ProcessID, result.ExitCode = &id, nil
		return result, nil
	}

	if storeErr != nil {
		result.Output = fmt.Sprintf("Error storing output: %v", storeErr)
	} else if storedOutput != nil {
		result.OutputSize = storedOutput.Size
		text, readTokens, truncated, err := r.readStoredOutputTail(*storedOutput, normalizeOutputTokens(maxTokens))
		if err != nil {
			return PersistentResult{}, err
		}
		result.Output = text
		result.OriginalTokenCount = storedOutput.TokenCount
		if result.OriginalTokenCount == 0 {
			result.OriginalTokenCount = readTokens
		}
		result.Truncated = truncated
	}
	return result, nil
}

func (r *Runner) persistentResultFallback(item *persistentProcess, wallTime time.Duration, running bool) PersistentResult {
	item.mu.Lock()
	output := item.output.drain()
	exitCode, storedOutput := item.exitCode, item.storedOutput
	item.mu.Unlock()
	result := PersistentResult{ChunkID: generateChunkID(), Name: item.name, WallTime: wallTime, ExitCode: exitCode}
	if running {
		result.Output, result.OmittedBytes, result.Truncated = output.text(), output.omitted, output.omitted > 0
		id := item.id
		result.ProcessID, result.ExitCode = &id, nil
	} else if storedOutput != nil {
		result.OutputPath, result.OutputSize = storedOutput.Path, storedOutput.Size
		result.Truncated = storedOutput.Size > 0
	}
	return result
}

func clampExecYield(value time.Duration) time.Duration {
	if value < MinYieldTime {
		return MinYieldTime
	}
	if value > MaxYieldTime {
		return MaxYieldTime
	}
	return value
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func normalizeOutputTokens(value *int) int {
	if value == nil {
		return DefaultOutputTokens
	}
	if *value > MaxOutputTokens {
		return MaxOutputTokens
	}
	return max(*value, 0)
}

func formatPersistentOutput(original, truncated string, originalTokens, omittedTokens, omittedBytes int) string {
	if omittedTokens == 0 {
		return truncated
	}
	if omittedBytes == 0 {
		return fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s", originalTokens, rustLineCount(original), truncated)
	}
	marker := fmt.Sprintf("... %d bytes omitted ...", omittedBytes)
	if !containsString(truncated, marker) {
		marker += "\n"
	} else {
		marker = ""
	}
	return fmt.Sprintf("Warning: truncated output (original token count: %d)\n%s\n%s", originalTokens, marker, truncated)
}

func rustLineCount(value string) int {
	if value == "" {
		return 0
	}
	count := 1
	for i := range len(value) {
		if value[i] == '\n' {
			count++
		}
	}
	if value[len(value)-1] == '\n' {
		count--
	}
	return count
}

func generateChunkID() string {
	var value [3]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return fmt.Sprintf("%06x", value)
}

func containsString(value, target string) bool {
	if target == "" {
		return true
	}
	for i := 0; i+len(target) <= len(value); i++ {
		if value[i:i+len(target)] == target {
			return true
		}
	}
	return false
}

type headTailBuffer struct {
	max, headBudget, tailBudget int
	head, tail                  []byte
	omitted                     int
}

func newHeadTailBuffer(max int) headTailBuffer {
	return headTailBuffer{max: max, headBudget: max / 2, tailBudget: max - max/2}
}

func (b *headTailBuffer) push(value []byte) {
	head := min(len(value), b.headBudget-len(b.head))
	if head > 0 {
		b.head = append(b.head, value[:head]...)
	}
	value = value[head:]
	if len(value) == 0 {
		return
	}
	b.tail = append(b.tail, value...)
	if excess := len(b.tail) - b.tailBudget; excess > 0 {
		b.tail = append([]byte(nil), b.tail[excess:]...)
		b.omitted += excess
	}
}

func (b *headTailBuffer) drain() headTailBuffer {
	result := *b
	*b = newHeadTailBuffer(b.max)
	return result
}

func (b headTailBuffer) totalBytes() int { return len(b.head) + len(b.tail) + b.omitted }

func (b headTailBuffer) text() string {
	if b.omitted == 0 {
		return string(bytesToValidUTF8(append(append([]byte(nil), b.head...), b.tail...)))
	}
	return string(bytesToValidUTF8(b.head)) + fmt.Sprintf("\n... %d bytes omitted ...\n", b.omitted) + string(bytesToValidUTF8(b.tail))
}

func bytesToValidUTF8(value []byte) []byte {
	if utf8.Valid(value) {
		return value
	}
	return []byte(strings.ToValidUTF8(string(value), "\uFFFD"))
}

func (r *Runner) reservePersistent(owner, requestedName string) (string, string, *persistentProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", "", nil, errors.New("process: runner is closed")
	}
	owned := 0
	for _, reservedOwner := range r.reservedIDs {
		if reservedOwner == owner {
			owned++
		}
	}
	if owned >= r.config.MaxSessionProcesses {
		r.reapFinishedLocked(owner)
		owned = 0
		for _, reservedOwner := range r.reservedIDs {
			if reservedOwner == owner {
				owned++
			}
		}
	}
	if owned >= r.config.MaxSessionProcesses {
		return "", "", nil, errors.New("process: session process limit reached")
	}
	if len(r.reservedIDs) >= r.config.MaxProcesses {
		r.reapFinishedLocked("")
	}
	if len(r.reservedIDs) >= r.config.MaxProcesses {
		return "", "", nil, errors.New("process: global process limit reached")
	}
	name := managedtask.SanitizeName(requestedName)
	if name == "" {
		name = managedtask.SanitizeName("shell-" + petname.Generate(2, "-"))
	}
	name = managedtask.UniqueName(name, func(candidate string) bool {
		for _, reservedName := range r.reservedNames {
			if reservedName == candidate {
				return false
			}
		}
		return true
	})
	for {
		processID, err := id.New("proc")
		if err != nil {
			return "", "", nil, err
		}
		if _, exists := r.reservedIDs[processID]; !exists {
			r.reservedIDs[processID] = owner
			r.reservedNames[processID] = name
			return processID, name, nil, nil
		}
	}
}

// reserveProcessID is retained for process-limit tests which do not care about
// the human-facing name.
func (r *Runner) reserveProcessID(owner string) (string, *persistentProcess, error) {
	processID, _, pruned, err := r.reservePersistent(owner, "")
	return processID, pruned, err
}

func (r *Runner) reapFinishedLocked(sessionID string) {
	for id, item := range r.processes {
		if sessionID != "" && item.sessionID != sessionID {
			continue
		}
		select {
		case <-item.finished:
			delete(r.processes, id)
			delete(r.reservedIDs, id)
			delete(r.reservedNames, id)
		default:
		}
	}
}

func (r *Runner) releaseReservedID(id string) {
	r.mu.Lock()
	delete(r.reservedIDs, id)
	delete(r.reservedNames, id)
	r.mu.Unlock()
}

func (r *Runner) storePersistent(item *persistentProcess) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		delete(r.reservedIDs, item.id)
		delete(r.reservedNames, item.id)
		return false
	}
	r.processes[item.id] = item
	return true
}

func (r *Runner) lookupPersistent(sessionID, identifier string) *persistentProcess {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if item := r.processes[identifier]; item != nil && item.sessionID == sessionID {
		return item
	}
	for _, item := range r.processes {
		if item.sessionID == sessionID && item.name == identifier {
			return item
		}
	}
	return nil
}

func (r *Runner) ownsPersistent(item *persistentProcess) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.processes[item.id] == item
}

func (r *Runner) removePersistent(item *persistentProcess) {
	r.mu.Lock()
	if r.processes[item.id] == item {
		delete(r.processes, item.id)
		delete(r.reservedIDs, item.id)
		delete(r.reservedNames, item.id)
	}
	r.mu.Unlock()
	if r.config.Tasks != nil {
		r.config.Tasks.Unregister(managedtask.Snapshot{SessionID: item.sessionID, ProcessID: item.id, Kind: managedtask.KindShell})
	}
}

func (r *Runner) terminatePersistent(item *persistentProcess) {
	item.terminate.Do(func() {
		pid := item.command.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		deadline := time.NewTimer(r.config.TerminationGrace)
		defer deadline.Stop()
		select {
		case <-item.finished:
			for groupExists(pid) {
				select {
				case <-deadline.C:
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		case <-deadline.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-item.finished
		}
	})
}

// ListActivePersistent returns active managed processes owned by one Parrot
// session, ordered by start time and then ID.
func (r *Runner) ListActivePersistent(sessionID string) []PersistentTask {
	r.mu.RLock()
	tasks := make([]PersistentTask, 0)
	for _, item := range r.processes {
		if item.sessionID != sessionID {
			continue
		}
		select {
		case <-item.finished:
			continue
		default:
			tasks = append(tasks, PersistentTask{ProcessID: item.id, Name: item.name, SessionID: item.sessionID, StartedAt: item.started})
		}
	}
	r.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].StartedAt.Equal(tasks[j].StartedAt) {
			return tasks[i].ProcessID < tasks[j].ProcessID
		}
		return tasks[i].StartedAt.Before(tasks[j].StartedAt)
	})
	return tasks
}

// InterruptPersistent claims and terminates one active retained process owned
// by a Parrot session.
func (r *Runner) InterruptPersistent(sessionID, processID string) (PersistentTask, error) {
	r.mu.Lock()
	item := r.processes[processID]
	if item == nil || item.sessionID != sessionID {
		item = nil
		for _, candidate := range r.processes {
			if candidate.sessionID == sessionID && candidate.name == processID {
				item = candidate
				break
			}
		}
	}
	if item == nil {
		r.mu.Unlock()
		return PersistentTask{}, fmt.Errorf("process: unknown process id or name %q", processID)
	}
	select {
	case <-item.finished:
		r.mu.Unlock()
		return PersistentTask{}, fmt.Errorf("process: unknown active process id %q", processID)
	default:
	}
	delete(r.processes, item.id)
	delete(r.reservedIDs, item.id)
	delete(r.reservedNames, item.id)
	r.mu.Unlock()
	if r.config.Tasks != nil {
		r.config.Tasks.Unregister(managedtask.Snapshot{SessionID: item.sessionID, ProcessID: item.id, Kind: managedtask.KindShell})
	}
	r.terminatePersistent(item)
	return PersistentTask{ProcessID: item.id, Name: item.name, SessionID: item.sessionID, StartedAt: item.started}, nil
}

type managedShellTask struct {
	runner  *Runner
	process *persistentProcess
}

func (t *managedShellTask) Snapshot() managedtask.Snapshot {
	t.process.mu.Lock()
	status := "running"
	select {
	case <-t.process.finished:
		status = "succeeded"
		if t.process.waitErr != nil || t.process.exitCode != nil && *t.process.exitCode != 0 {
			status = "failed"
		}
	default:
	}
	snapshot := managedtask.Snapshot{ProcessID: t.process.id, Name: t.process.name, SessionID: t.process.sessionID, Kind: managedtask.KindShell, Status: status, StartedAt: t.process.started}
	t.process.mu.Unlock()
	return snapshot
}

func (t *managedShellTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	completion, err := (&PersistentObserver{process: t.process}).Wait(ctx)
	if err != nil {
		return managedtask.Completion{}, err
	}
	result := managedtask.Completion{Task: t.Snapshot(), ExitCode: completion.ExitCode}
	if completion.WaitError != nil {
		result.Error = completion.WaitError.Error()
	}
	return result, nil
}

func (t *managedShellTask) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	item, err := t.runner.InterruptPersistent(t.process.sessionID, t.process.id)
	if err != nil {
		return managedtask.Snapshot{}, err
	}
	select {
	case <-t.process.finished:
	case <-ctx.Done():
		return managedtask.Snapshot{}, ctx.Err()
	}
	return managedtask.Snapshot{ProcessID: item.ProcessID, Name: item.Name, SessionID: item.SessionID, Kind: managedtask.KindShell, Status: "canceled", StartedAt: item.StartedAt}, nil
}

// SuspendSession prevents new shell-task notifications for one session,
// cancels in-flight delivery, and waits until delivery has stopped.
func (r *Runner) SuspendSession(ctx context.Context, sessionID string) error {
	r.mu.Lock()
	r.notifyPaused[sessionID]++
	active := make([]*activeNotification, 0, len(r.notifications[sessionID]))
	for _, notification := range r.notifications[sessionID] {
		notification.cancel()
		active = append(active, notification)
	}
	r.mu.Unlock()
	for _, notification := range active {
		select {
		case <-notification.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ResumeSession permits shell-task notifications after lifecycle cleanup.
func (r *Runner) ResumeSession(sessionID string) {
	r.mu.Lock()
	if r.notifyPaused[sessionID] <= 1 {
		delete(r.notifyPaused, sessionID)
	} else {
		r.notifyPaused[sessionID]--
	}
	r.mu.Unlock()
}

// InterruptSession terminates retained processes owned by one Parrot session.
func (r *Runner) InterruptSession(sessionID string) error {
	items := r.takeSessionProcesses(sessionID, false)
	for _, item := range items {
		if r.config.Tasks != nil {
			r.config.Tasks.Unregister(managedtask.Snapshot{SessionID: item.sessionID, ProcessID: item.id, Kind: managedtask.KindShell})
		}
		r.terminatePersistent(item)
	}
	return nil
}

// DeleteSession terminates retained processes and forgets sandbox write grants.
func (r *Runner) DeleteSession(sessionID string) error {
	r.cleanup.Lock()
	defer r.cleanup.Unlock()
	temporaryDirectory, wait := r.beginTemporaryDirectoryDeletion(sessionID)
	if wait != nil {
		<-wait
	}
	items := r.takeSessionProcesses(sessionID, true)
	for _, item := range items {
		if r.config.Tasks != nil {
			r.config.Tasks.Unregister(managedtask.Snapshot{SessionID: item.sessionID, ProcessID: item.id, Kind: managedtask.KindShell})
		}
		r.terminatePersistent(item)
	}
	if temporaryDirectory != "" {
		if err := removeTemporaryDirectory(temporaryDirectory); err != nil {
			return err
		}
		r.mu.Lock()
		delete(r.temporaryDirs, sessionID)
		r.mu.Unlock()
	}
	return nil
}

func (r *Runner) beginTemporaryDirectoryDeletion(sessionID string) (string, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletedSessions[sessionID] = struct{}{}
	state := r.temporaryDirs[sessionID]
	if state == nil {
		return "", nil
	}
	state.deleting = true
	if state.users == 0 {
		return state.path, nil
	}
	return state.path, state.idle
}

func (r *Runner) takeSessionProcesses(sessionID string, deleteGrants bool) []*persistentProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	var items []*persistentProcess
	for id, item := range r.processes {
		if item.sessionID == sessionID {
			items = append(items, item)
			delete(r.processes, id)
			delete(r.reservedIDs, id)
			delete(r.reservedNames, id)
		}
	}
	if deleteGrants {
		delete(r.writablePaths, sessionID)
	}
	return items
}

// Close terminates every retained process and prevents new persistent starts.
func (r *Runner) Close() error {
	if r == nil {
		return nil
	}
	r.cleanup.Lock()
	defer r.cleanup.Unlock()
	r.mu.Lock()
	r.closed = true
	r.notifyCancel()
	items := make([]*persistentProcess, 0, len(r.processes))
	for _, item := range r.processes {
		items = append(items, item)
	}
	r.processes = make(map[string]*persistentProcess)
	r.reservedIDs = make(map[string]string)
	r.reservedNames = make(map[string]string)
	r.writablePaths = make(map[string]map[string]struct{})
	r.notifyPaused = make(map[string]int)
	type temporaryDirectoryCleanup struct {
		path string
		wait <-chan struct{}
	}
	temporaryDirectories := make(map[string]temporaryDirectoryCleanup, len(r.temporaryDirs))
	for sessionID, state := range r.temporaryDirs {
		state.deleting = true
		var wait <-chan struct{}
		if state.users > 0 {
			wait = state.idle
		}
		temporaryDirectories[sessionID] = temporaryDirectoryCleanup{path: state.path, wait: wait}
	}
	r.mu.Unlock()
	for _, item := range items {
		if r.config.Tasks != nil {
			r.config.Tasks.Unregister(managedtask.Snapshot{SessionID: item.sessionID, ProcessID: item.id, Kind: managedtask.KindShell})
		}
		r.terminatePersistent(item)
	}
	r.notifyWG.Wait()
	var err error
	for sessionID, temporaryDirectory := range temporaryDirectories {
		if temporaryDirectory.wait != nil {
			<-temporaryDirectory.wait
		}
		if removeErr := removeTemporaryDirectory(temporaryDirectory.path); removeErr != nil {
			err = errors.Join(err, removeErr)
			continue
		}
		r.mu.Lock()
		delete(r.temporaryDirs, sessionID)
		r.mu.Unlock()
	}
	return err
}

func removeTemporaryDirectory(path string) error {
	if err := filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := clearTemporaryDirectoryFlags(path, entry); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("process: prepare session temporary directory for removal: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("process: remove session temporary directory: %w", err)
	}
	return nil
}
