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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

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
	SessionID       string
	Yield           time.Duration
	MaxOutputTokens *int
	TTY             bool
	Output          io.Writer
	Unrestricted    bool
}

type PersistentWriteRequest struct {
	SessionID       string
	ProcessID       int32
	Chars           string
	Yield           time.Duration
	MaxOutputTokens *int
	Output          io.Writer
}

type PersistentResult struct {
	ChunkID            string
	WallTime           time.Duration
	Output             string
	ProcessID          *int32
	ExitCode           *int
	OriginalTokenCount int
	OmittedBytes       int
}

type persistentProcess struct {
	id          int32
	owner       string
	command     *exec.Cmd
	stdin       io.WriteCloser
	reader      io.ReadCloser
	tty         bool
	started     time.Time
	lastUsed    time.Time
	interaction sync.Mutex
	terminate   sync.Once

	mu         sync.Mutex
	output     headTailBuffer
	stream     io.Writer
	waitErr    error
	exitCode   *int
	notify     chan struct{}
	finished   chan struct{}
	readerDone chan struct{}
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
	environment, err := r.environment(map[string]string{
		"NO_COLOR": "1", "TERM": "dumb", "LANG": "C.UTF-8", "LC_CTYPE": "C.UTF-8", "LC_ALL": "C.UTF-8",
		"COLORTERM": "", "PAGER": "cat", "GIT_PAGER": "cat", "GH_PAGER": "cat", "CODEX_CI": "1",
	})
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
		writablePaths, writableErr := r.writableForSession(request.SessionID)
		if writableErr != nil {
			return PersistentResult{}, writableErr
		}
		program, arguments, err = r.sandbox.command(resolvedShell, request.Command, resolved, writablePaths, temporaryDirectory)
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
	command := exec.Command(program, arguments...)
	command.Dir, command.Env = resolved, environment
	item := &persistentProcess{
		id: processID, owner: request.SessionID, command: command, tty: request.TTY,
		started: time.Now(), lastUsed: time.Now(), output: newHeadTailBuffer(persistentOutputBytes),
		notify: make(chan struct{}, 1), finished: make(chan struct{}), readerDone: make(chan struct{}),
	}
	if err := r.startPersistent(item); err != nil {
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
	}
	return result, nil
}

// WritePersistent writes to or polls an existing process. Pipe-backed
// processes accept only Ctrl-C, matching Codex unified exec behavior.
func (r *Runner) WritePersistent(ctx context.Context, request PersistentWriteRequest) (PersistentResult, error) {
	item := r.lookupPersistent(request.SessionID, request.ProcessID)
	if item == nil {
		return PersistentResult{}, fmt.Errorf("process: unknown process id %d", request.ProcessID)
	}
	item.interaction.Lock()
	defer item.interaction.Unlock()
	if !r.ownsPersistent(item) {
		return PersistentResult{}, fmt.Errorf("process: unknown process id %d", request.ProcessID)
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

func (r *Runner) startPersistent(item *persistentProcess) error {
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
		item.mu.Lock()
		item.waitErr = err
		if item.command.ProcessState != nil {
			code := item.command.ProcessState.ExitCode()
			item.exitCode = &code
		}
		item.mu.Unlock()
		close(item.finished)
		item.signalOutput()
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
			return persistentResult(item, time.Since(started), false, maxTokens), nil
		case <-timer.C:
			select {
			case <-item.finished:
				return persistentResult(item, time.Since(started), false, maxTokens), nil
			default:
				return persistentResult(item, time.Since(started), true, maxTokens), nil
			}
		case <-ctx.Done():
			return PersistentResult{}, ctx.Err()
		}
	}
}

func persistentResult(item *persistentProcess, wallTime time.Duration, running bool, maxTokens *int) PersistentResult {
	item.mu.Lock()
	output := item.output.drain()
	exitCode := item.exitCode
	item.mu.Unlock()
	result := PersistentResult{
		ChunkID: generateChunkID(), WallTime: wallTime,
		Output: output.text(), ExitCode: exitCode,
		OriginalTokenCount: tokensForBytes(output.totalBytes()), OmittedBytes: output.omitted,
	}
	result.Output = truncatePersistentOutput(result.Output, maxTokens, result.OriginalTokenCount, result.OmittedBytes)
	if running {
		id := item.id
		result.ProcessID, result.ExitCode = &id, nil
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

func (r *Runner) reserveProcessID(owner string) (int32, *persistentProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, nil, errors.New("process: runner is closed")
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
		return 0, nil, errors.New("process: session process limit reached")
	}
	if len(r.reservedIDs) >= r.config.MaxProcesses {
		r.reapFinishedLocked("")
	}
	if len(r.reservedIDs) >= r.config.MaxProcesses {
		return 0, nil, errors.New("process: global process limit reached")
	}
	for {
		var raw [4]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, nil, err
		}
		id := int32(uint32(raw[0])<<24|uint32(raw[1])<<16|uint32(raw[2])<<8|uint32(raw[3])) & math.MaxInt32
		id = 1000 + id%99000
		if _, exists := r.reservedIDs[id]; !exists {
			r.reservedIDs[id] = owner
			return id, nil, nil
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

func (r *Runner) releaseReservedID(id int32) {
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

func (r *Runner) lookupPersistent(owner string, id int32) *persistentProcess {
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

// InterruptSession terminates retained processes owned by one Parrot session.
func (r *Runner) InterruptSession(sessionID string) error {
	items := r.takeSessionProcesses(sessionID, false)
	for _, item := range items {
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
	r.processes = make(map[int32]*persistentProcess)
	r.reservedIDs = make(map[int32]string)
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
