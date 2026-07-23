package process

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/security"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/creack/pty"
)

const (
	MinYieldTime          = 250 * time.Millisecond
	MinEmptyPollYieldTime = 5 * time.Second
	MaxYieldTime          = 30 * time.Second
	MaxEmptyPollYieldTime = 5 * time.Minute
	DefaultExecYieldTime  = 10 * time.Second
	DefaultWriteYieldTime = 250 * time.Millisecond
	DefaultOutputTokens   = 10_000
	MaxOutputTokens       = (1 << 20) / 4
	persistentOutputBytes = 1 << 20
)

type PersistentRequest struct {
	Shell           string
	Command         string
	Cwd             string
	Env             map[string]string
	SessionID       string
	ParentTaskID    string
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
	WallTime           time.Duration
	Output             string
	ProcessID          *string
	ExitCode           *int
	OriginalTokenCount int
	OmittedBytes       int
	OutputID           string
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

// PersistentTask identifies an active managed process and when it started.
type PersistentTask struct {
	ID           string
	ParentTaskID string
	StartedAt    time.Time
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
	owner        string
	parentTaskID string
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

	mu            sync.Mutex
	output        headTailBuffer
	stream        io.Writer
	waitErr       error
	exitCode      *int
	managedOutput ManagedOutput
	storedOutput  *StoredOutput
	storeErr      error
	notify        chan struct{}
	finished      chan struct{}
	readerDone    chan struct{}
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
		profile, buildErr := r.buildProfile(request.SecurityProfile, request.SessionID, resolved, temporaryDirectory)
		if buildErr != nil {
			return PersistentResult{}, buildErr
		}
		program, arguments, err = r.sandbox.command(resolvedShell, request.Command, resolved, profile, temporaryDirectory)
		if err != nil {
			return PersistentResult{}, fmt.Errorf("process: sandbox: %w", err)
		}
	}

	processID, pruned, err := r.reserveProcessID(request.SessionID)
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
	managedOutput, err := r.config.OutputStore.Create(ctx)
	if err != nil {
		r.releaseReservedID(processID)
		return PersistentResult{}, fmt.Errorf("process: create output: %w", err)
	}
	command := exec.Command(program, arguments...)
	command.Dir, command.Env = resolved, environment
	item := &persistentProcess{
		id: processID, owner: request.SessionID, parentTaskID: request.ParentTaskID, command: command, tty: request.TTY,
		started: time.Now(), lastUsed: time.Now(), output: newHeadTailBuffer(persistentOutputBytes),
		managedOutput: managedOutput,
		notify:        make(chan struct{}, 1), finished: make(chan struct{}), readerDone: make(chan struct{}),
	}
	if err := r.startPersistent(ctx, item); err != nil {
		managedOutput.Discard()
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
			if err := r.config.Tasks.Register(&managedShellTask{runner: r, process: item}, func(caller string) bool { return caller == item.owner }); err != nil {
				r.removePersistent(item)
				r.terminatePersistent(item)
				return PersistentResult{}, err
			}
		}
		r.announcePersistent(item)
	}
	return result, nil
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
	r.emitPersistent(PersistentEvent{Kind: PersistentEventStart, SessionID: item.owner, TaskID: item.id, ParentTaskID: item.parentTaskID, StartedAt: started})
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
	event := PersistentEvent{Kind: PersistentEventFinished, SessionID: item.owner, TaskID: item.id, ParentTaskID: item.parentTaskID, StartedAt: item.started, ExitCode: exitCode}
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
		stored, storeErr := item.managedOutput.Finalize(context.WithoutCancel(ctx))
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
	if w.process.managedOutput != nil {
		_, _ = w.process.managedOutput.Write(value)
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
			return r.persistentResult(item, time.Since(started), false, maxTokens), nil
		case <-timer.C:
			select {
			case <-item.finished:
				return r.persistentResult(item, time.Since(started), false, maxTokens), nil
			default:
				return r.persistentResult(item, time.Since(started), true, maxTokens), nil
			}
		case <-ctx.Done():
			return PersistentResult{}, ctx.Err()
		}
	}
}

func (r *Runner) persistentResult(item *persistentProcess, wallTime time.Duration, running bool, maxTokens *int) PersistentResult {
	item.mu.Lock()
	output := item.output.drain()
	exitCode := item.exitCode
	var managedOutput ManagedOutput
	if item.managedOutput != nil {
		managedOutput = item.managedOutput
	}
	storedOutput := item.storedOutput
	storeErr := item.storeErr
	item.mu.Unlock()

	outputID := ""
	if managedOutput != nil {
		outputID = managedOutput.ID()
	}
	result := PersistentResult{
		ChunkID: generateChunkID(), WallTime: wallTime,
		ExitCode: exitCode, OutputID: outputID,
	}

	if running {
		result.Output = truncatePersistentOutput(output.text(), maxTokens, tokensForBytes(output.totalBytes()), output.omitted)
		result.OriginalTokenCount = tokensForBytes(output.totalBytes())
		result.OmittedBytes = output.omitted
		id := item.id
		result.ProcessID, result.ExitCode = &id, nil
		return result
	}

	if storeErr != nil {
		result.Output = fmt.Sprintf("Error storing output: %v", storeErr)
	} else if storedOutput != nil {
		budget := int64(normalizeOutputTokens(maxTokens) * 4)
		text, truncated, err := r.readStoredOutputWithBudget(*storedOutput, budget)
		if err != nil {
			result.Output = fmt.Sprintf("Error reading output: %v", err)
		} else {
			result.Output = text
			result.Truncated = truncated
			result.OutputSize = storedOutput.Size
			result.OriginalTokenCount = tokensForBytes(int(storedOutput.Size))
			result.OmittedBytes = int(storedOutput.OmittedBytes)
		}
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

func normalizeOutputTokens(value *int) int {
	if value == nil {
		return DefaultOutputTokens
	}
	if *value > MaxOutputTokens {
		return MaxOutputTokens
	}
	return max(*value, 0)
}

func truncatePersistentOutput(text string, requestedTokens *int, originalTokens, omittedBytes int) string {
	maxTokens := normalizeOutputTokens(requestedTokens)
	budget := maxTokens * 4
	if omittedBytes == 0 {
		if len(text) <= budget {
			return text
		}
		return fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s", originalTokens, rustLineCount(text), truncateMiddleTokens(text, maxTokens))
	}
	marker := fmt.Sprintf("... %d bytes omitted ...", omittedBytes)
	if len(text) <= budget {
		return text
	}
	truncated := truncateMiddleTokens(text, maxTokens)
	if !containsString(truncated, marker) {
		marker += "\n"
	} else {
		marker = ""
	}
	return fmt.Sprintf("Warning: truncated output (original token count: %d)\n%s\n%s", originalTokens, marker, truncated)
}

func truncateMiddleTokens(value string, tokens int) string {
	budget := tokens * 4
	if budget > 0 && len(value) <= budget {
		return value
	}
	leftBudget, rightBudget := budget/2, budget-budget/2
	left := validPrefix(value, leftBudget)
	right := validSuffix(value, rightBudget)
	removed := len(value) - len(left) - len(right)
	return left + fmt.Sprintf("…%d tokens truncated…", tokensForBytes(removed)) + right
}

func validPrefix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func validSuffix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.ValidString(value[start:]) {
		start++
	}
	return value[start:]
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

func tokensForBytes(bytes int) int { return int(math.Ceil(float64(bytes) / 4)) }

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

func (r *Runner) reserveProcessID(owner string) (string, *persistentProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", nil, errors.New("process: runner is closed")
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
		return "", nil, errors.New("process: session process limit reached")
	}
	if len(r.reservedIDs) >= r.config.MaxProcesses {
		r.reapFinishedLocked("")
	}
	if len(r.reservedIDs) >= r.config.MaxProcesses {
		return "", nil, errors.New("process: global process limit reached")
	}
	for {
		processID, err := id.New("proc")
		if err != nil {
			return "", nil, err
		}
		if _, exists := r.reservedIDs[processID]; !exists {
			r.reservedIDs[processID] = owner
			return processID, nil, nil
		}
	}
}

func (r *Runner) reapFinishedLocked(owner string) {
	for id, item := range r.processes {
		if owner != "" && item.owner != owner {
			continue
		}
		select {
		case <-item.finished:
			delete(r.processes, id)
			delete(r.reservedIDs, id)
		default:
		}
	}
}

func (r *Runner) releaseReservedID(id string) {
	r.mu.Lock()
	delete(r.reservedIDs, id)
	r.mu.Unlock()
}

func (r *Runner) storePersistent(item *persistentProcess) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		delete(r.reservedIDs, item.id)
		return false
	}
	r.processes[item.id] = item
	return true
}

func (r *Runner) lookupPersistent(owner, id string) *persistentProcess {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item := r.processes[id]
	if item == nil || item.owner != owner {
		return nil
	}
	return item
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
	}
	r.mu.Unlock()
	if r.config.Tasks != nil {
		r.config.Tasks.Unregister(item.id)
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
func (r *Runner) ListActivePersistent(owner string) []PersistentTask {
	r.mu.RLock()
	tasks := make([]PersistentTask, 0)
	for _, item := range r.processes {
		if item.owner != owner {
			continue
		}
		select {
		case <-item.finished:
			continue
		default:
			tasks = append(tasks, PersistentTask{ID: item.id, ParentTaskID: item.parentTaskID, StartedAt: item.started})
		}
	}
	r.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].StartedAt.Equal(tasks[j].StartedAt) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].StartedAt.Before(tasks[j].StartedAt)
	})
	return tasks
}

// InterruptPersistent claims and terminates one active retained process owned
// by a Parrot session.
func (r *Runner) InterruptPersistent(owner, processID string) (PersistentTask, error) {
	r.mu.Lock()
	item := r.processes[processID]
	if item == nil || item.owner != owner {
		r.mu.Unlock()
		return PersistentTask{}, fmt.Errorf("process: unknown process id %q", processID)
	}
	select {
	case <-item.finished:
		r.mu.Unlock()
		return PersistentTask{}, fmt.Errorf("process: unknown active process id %q", processID)
	default:
	}
	delete(r.processes, processID)
	delete(r.reservedIDs, processID)
	r.mu.Unlock()
	if r.config.Tasks != nil {
		r.config.Tasks.Unregister(item.id)
	}
	r.terminatePersistent(item)
	return PersistentTask{ID: item.id, ParentTaskID: item.parentTaskID, StartedAt: item.started}, nil
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
	snapshot := managedtask.Snapshot{ID: t.process.id, ParentTaskID: t.process.parentTaskID, Kind: managedtask.KindShell, Status: status, StartedAt: t.process.started}
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
	item, err := t.runner.InterruptPersistent(t.process.owner, t.process.id)
	if err != nil {
		return managedtask.Snapshot{}, err
	}
	select {
	case <-t.process.finished:
	case <-ctx.Done():
		return managedtask.Snapshot{}, ctx.Err()
	}
	return managedtask.Snapshot{ID: item.ID, ParentTaskID: item.ParentTaskID, Kind: managedtask.KindShell, Status: "canceled", StartedAt: item.StartedAt}, nil
}

// InterruptSession terminates retained processes owned by one Parrot session.
func (r *Runner) InterruptSession(sessionID string) error {
	items := r.takeSessionProcesses(sessionID, false)
	for _, item := range items {
		if r.config.Tasks != nil {
			r.config.Tasks.Unregister(item.id)
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
			r.config.Tasks.Unregister(item.id)
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
		if item.owner == sessionID {
			items = append(items, item)
			delete(r.processes, id)
			delete(r.reservedIDs, id)
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
	items := make([]*persistentProcess, 0, len(r.processes))
	for _, item := range r.processes {
		items = append(items, item)
	}
	r.processes = make(map[string]*persistentProcess)
	r.reservedIDs = make(map[string]string)
	r.writablePaths = make(map[string]map[string]struct{})
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
			r.config.Tasks.Unregister(item.id)
		}
		r.terminatePersistent(item)
	}
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
