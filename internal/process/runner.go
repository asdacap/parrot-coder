// Package process runs bounded, cancellable shell commands in a workspace.
package process

import (
	"context"
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

	"github.com/amirulashraf/parrot-coder/internal/security"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type Config struct {
	Workspace              *workspace.Workspace
	WorkingDirectory       string
	Environment            map[string]string
	MaxOutputBytes         int64
	Timeout                time.Duration
	TerminationGrace       time.Duration
	OutputStore            OutputStore
	AllowUnsafeEnvironment bool
	MaxProcesses           int
	MaxSessionProcesses    int
	OnPersistentEvent      func(PersistentEvent)
	AgentSessions          AgentSessionResolver
	Tasks                  *managedtask.Manager
	SandboxRules           []security.Rule
	sandbox                sandbox
}

// AgentSession is the capability used to deliver a yielded process's eventual
// completion to its owning session.
type AgentSession interface {
	Send(context.Context, string) (string, error)
}

// AgentSessionResolver resolves the owner of a yielded process.
type AgentSessionResolver interface {
	Get(string) AgentSession
}

// Persistent lifecycle kinds reported through Config.OnPersistentEvent.
const (
	PersistentEventStart    = "start"
	PersistentEventFinished = "finished"
)

// PersistentEvent is one flat shell-task lifecycle emission. A shell task is a
// persistent process which outlived the command that started it.
type PersistentEvent struct {
	Kind      string
	SessionID string
	TaskID    string
	Name      string
	StartedAt time.Time
	ExitCode  *int
	Error     string
}

type StoredOutput struct {
	ID           string
	Size         int64
	OmittedBytes int64
	Preview      string
}

type OutputStore interface {
	Create(context.Context) (ManagedOutput, error)
	Read(id string, offset, limit int64) ([]byte, error)
}

type ManagedOutput interface {
	io.Writer
	ID() string
	Finalize(ctx context.Context) (StoredOutput, error)
	Discard()
}

type Request struct {
	Shell           string            `json:"shell"`
	Command         string            `json:"command"`
	Cwd             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Timeout         time.Duration     `json:"timeout,omitempty"`
	Output          io.Writer         `json:"-"`
	SessionID       string            `json:"-"`
	SecurityProfile security.SecurityProfile
}

type Result struct {
	Output     string `json:"output"`
	OutputTail string `json:"output_tail"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Cancelled  bool   `json:"cancelled"`
	Truncated  bool   `json:"truncated"`
	OutputID   string `json:"output_id,omitempty"`
	OutputSize int64  `json:"output_size,omitempty"`
}

type Runner struct {
	config          Config
	sandbox         sandbox
	mu              sync.RWMutex
	cleanup         sync.Mutex
	writablePaths   map[string]map[string]struct{}
	temporaryDirs   map[string]*sessionTemporaryDirectory
	deletedSessions map[string]struct{}
	processes       map[string]*persistentProcess
	reservedIDs     map[string]string
	reservedNames   map[string]string
	notifyCtx       context.Context
	notifyCancel    context.CancelFunc
	notifications   map[string]map[string]*activeNotification
	notifyPaused    map[string]int
	notifyWG        sync.WaitGroup
	closed          bool
}

// DefaultShell returns the user's configured shell when it is an absolute,
// executable regular file, and otherwise falls back to the system POSIX shell.
func DefaultShell() (string, error) {
	for _, candidate := range []string{os.Getenv("SHELL"), "/bin/sh"} {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("process: could not detect an executable shell")
}

// ResolveWorkingDirectory canonicalizes an existing directory. Relative paths
// remain workspace-relative, while absolute paths may point anywhere.
func ResolveWorkingDirectory(path, workspaceRoot string) (string, error) {
	if path == "" {
		path = workspaceRoot
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(workspaceRoot, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("working directory is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func NewRunner(config Config) (*Runner, error) {
	if config.Workspace == nil {
		return nil, errors.New("process: workspace is required")
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = 64 << 10
	}
	if config.Timeout <= 0 {
		config.Timeout = 2 * time.Minute
	}
	if config.TerminationGrace <= 0 {
		config.TerminationGrace = 500 * time.Millisecond
	}
	if config.WorkingDirectory == "" {
		config.WorkingDirectory = config.Workspace.Root()
	}
	if config.MaxProcesses <= 0 {
		config.MaxProcesses = 64
	}
	if config.MaxSessionProcesses <= 0 {
		config.MaxSessionProcesses = 64
	}
	implementation := config.sandbox
	if implementation == nil {
		workingDirectory := config.WorkingDirectory
		if workingDirectory == "" {
			workingDirectory = config.Workspace.Root()
		}
		resolved, err := config.Workspace.ResolveRead(workingDirectory)
		if err != nil {
			return nil, fmt.Errorf("process: resolve working directory: %w", err)
		}
		implementation = platformSandbox(config.Workspace, resolved)
	}
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	return &Runner{
		config: config, sandbox: implementation, notifyCtx: notifyCtx, notifyCancel: notifyCancel,
		writablePaths: make(map[string]map[string]struct{}),
		temporaryDirs: make(map[string]*sessionTemporaryDirectory), deletedSessions: make(map[string]struct{}),
		processes: make(map[string]*persistentProcess), reservedIDs: make(map[string]string), reservedNames: make(map[string]string),
		notifications: make(map[string]map[string]*activeNotification), notifyPaused: make(map[string]int),
	}, nil
}

// WorkingDirectory returns the canonical default cwd used by process tools.
func (r *Runner) WorkingDirectory() string { return r.config.WorkingDirectory }

// SetPersistentEventHandler installs the shell-task lifecycle observer after
// composition, when the event sink exists. It replaces any previous handler.
func (r *Runner) SetPersistentEventHandler(handler func(PersistentEvent)) {
	r.mu.Lock()
	r.config.OnPersistentEvent = handler
	r.mu.Unlock()
}

func (r *Runner) persistentEventHandler() func(PersistentEvent) {
	r.mu.RLock()
	handler := r.config.OnPersistentEvent
	r.mu.RUnlock()
	return handler
}

// SetAgentSessions completes composition after the agent coordinator is
// available. Processes started before composition do not send notifications.
func (r *Runner) SetAgentSessions(sessions AgentSessionResolver) {
	r.mu.Lock()
	r.config.AgentSessions = sessions
	r.mu.Unlock()
}

func (r *Runner) agentSessions() AgentSessionResolver {
	r.mu.RLock()
	sessions := r.config.AgentSessions
	r.mu.RUnlock()
	return sessions
}

// AllowWrite grants sandboxed commands in one session write access to an exact
// existing file or directory. Grants are held only for this Runner's lifetime.
func (r *Runner) AllowWrite(sessionID, path string) error {
	if sessionID == "" || !filepath.IsAbs(path) {
		return errors.New("process: session and absolute writable path are required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("process: resolve writable path: %w", err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("process: stat writable path: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("process: runner is closed")
	}
	if _, deleted := r.deletedSessions[sessionID]; deleted {
		return errors.New("process: session is deleted")
	}
	if r.writablePaths[sessionID] == nil {
		r.writablePaths[sessionID] = make(map[string]struct{})
	}
	for existing := range r.writablePaths[sessionID] {
		if isPathWithin(resolved, existing) {
			return nil
		}
		if isPathWithin(existing, resolved) {
			delete(r.writablePaths[sessionID], existing)
		}
	}
	r.writablePaths[sessionID][resolved] = struct{}{}
	return nil
}

func isPathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (r *Runner) writableForSession(sessionID string) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	paths := make([]string, 0, len(r.writablePaths[sessionID]))
	for path := range r.writablePaths[sessionID] {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return nil, fmt.Errorf("process: writable path changed after approval: %s", path)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

type sessionTemporaryDirectory struct {
	path     string
	users    int
	idle     chan struct{}
	deleting bool
}

func (r *Runner) acquireTemporaryDirectory(sessionID string) (string, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", nil, errors.New("process: runner is closed")
	}
	if _, deleted := r.deletedSessions[sessionID]; deleted {
		return "", nil, errors.New("process: session is deleted")
	}
	state := r.temporaryDirs[sessionID]
	if state == nil {
		path, err := os.MkdirTemp("", "parrot-session-*")
		if err != nil {
			return "", nil, fmt.Errorf("process: create session temporary directory: %w", err)
		}
		resolvedPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			_ = os.RemoveAll(path)
			return "", nil, fmt.Errorf("process: resolve session temporary directory: %w", err)
		}
		path = resolvedPath
		state = &sessionTemporaryDirectory{path: path}
		r.temporaryDirs[sessionID] = state
	}
	if state.deleting {
		return "", nil, errors.New("process: session is being deleted")
	}
	if state.users == 0 {
		state.idle = make(chan struct{})
	}
	state.users++
	var once sync.Once
	release := func() {
		once.Do(func() {
			r.mu.Lock()
			state.users--
			if state.users == 0 {
				close(state.idle)
			}
			r.mu.Unlock()
		})
	}
	return state.path, release, nil
}

// Run treats the command as arbitrary process execution. The shell path is an
// executable, not a policy boundary. If omitted, it is detected automatically.
func (r *Runner) Run(ctx context.Context, request Request) (Result, error) {
	return r.run(ctx, request, true)
}

// RunUnrestricted executes without the platform sandbox. Callers must obtain
// explicit permission before invoking it.
func (r *Runner) RunUnrestricted(ctx context.Context, request Request) (Result, error) {
	return r.run(ctx, request, false)
}

func (r *Runner) run(ctx context.Context, request Request, sandboxed bool) (Result, error) {
	if request.Shell == "" {
		var err error
		request.Shell, err = DefaultShell()
		if err != nil {
			return Result{}, err
		}
	}
	if !filepath.IsAbs(request.Shell) || request.Command == "" {
		return Result{}, errors.New("process: absolute shell path and command are required")
	}
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1, Cancelled: true}, err
	}
	resolved, err := ResolveWorkingDirectory(request.Cwd, r.config.Workspace.Root())
	if err != nil {
		return Result{}, fmt.Errorf("process: resolve cwd: %w", err)
	}
	environment, err := r.environment(request.Env)
	if err != nil {
		return Result{}, err
	}

	if r.config.OutputStore == nil {
		return Result{}, errors.New("process: output store is required")
	}
	managedOutput, err := r.config.OutputStore.Create(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("process: create output: %w", err)
	}
	capture := &streamWriter{stream: request.Output, output: managedOutput}
	fail := func(err error) (Result, error) {
		managedOutput.Discard()
		return Result{}, err
	}
	resolvedShell, err := executableFile(request.Shell)
	if err != nil {
		return fail(fmt.Errorf("process: shell: %w", err))
	}
	program, arguments := resolvedShell, []string{"-c", request.Command}
	if sandboxed {
		temporaryDirectory, releaseTemporaryDirectory, temporaryErr := r.acquireTemporaryDirectory(request.SessionID)
		if temporaryErr != nil {
			return fail(temporaryErr)
		}
		defer releaseTemporaryDirectory()
		setEnvironment(environment, "TMPDIR", r.sandbox.temporaryDirectory(temporaryDirectory))
		profile, buildErr := r.buildProfile(request.SecurityProfile, request.SessionID, resolved)
		if buildErr != nil {
			return fail(buildErr)
		}
		program, arguments, err = r.sandbox.command(resolvedShell, request.Command, resolved, profile, temporaryDirectory)
		if err != nil {
			return fail(fmt.Errorf("process: sandbox: %w", err))
		}
	}
	command := exec.Command(program, arguments...)
	command.Dir = resolved
	command.Env = environment
	command.Stdin = nil
	command.Stdout = capture
	command.Stderr = capture
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return fail(fmt.Errorf("process: start: %w", err))
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()

	timeout := request.Timeout
	if timeout <= 0 || timeout > r.config.Timeout {
		timeout = r.config.Timeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr error
	result := Result{ExitCode: -1}
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		result.Cancelled = true
		waitErr = r.terminate(command.Process.Pid, wait)
	case <-timer.C:
		result.TimedOut = true
		waitErr = r.terminate(command.Process.Pid, wait)
	}
	storedOutput, storeErr := managedOutput.Finalize(context.WithoutCancel(ctx))
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	result.OutputTail = capture.tail.String()
	if storeErr != nil {
		return result, fmt.Errorf("process: store output: %w", storeErr)
	}
	if capture.err != nil {
		return result, fmt.Errorf("process: capture output: %w", capture.err)
	}
	result.OutputID = storedOutput.ID
	result.OutputSize = storedOutput.Size
	output, truncated, readErr := r.readStoredOutput(storedOutput)
	if readErr != nil {
		return result, fmt.Errorf("process: read output: %w", readErr)
	}
	result.Output, result.Truncated = output, truncated
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return result, fmt.Errorf("process: wait: %w", waitErr)
	}
	return result, nil
}

// readStoredOutput bounds stored command output to the model-facing budget,
// keeping the head and tail and reporting bytes the store dropped up front.
func (r *Runner) readStoredOutputWithBudget(stored StoredOutput, budget int64) (string, bool, error) {
	if budget <= 0 {
		budget = r.config.MaxOutputBytes
	}
	truncated := stored.OmittedBytes > 0
	var text string
	if stored.Size <= budget {
		data, err := r.config.OutputStore.Read(stored.ID, 0, stored.Size)
		if err != nil {
			return "", false, err
		}
		text = string(bytesToValidUTF8(data))
	} else {
		half := budget / 2
		head, err := r.config.OutputStore.Read(stored.ID, 0, half)
		if err != nil {
			return "", false, err
		}
		tail, err := r.config.OutputStore.Read(stored.ID, stored.Size-half, half)
		if err != nil {
			return "", false, err
		}
		text = string(bytesToValidUTF8(head)) + fmt.Sprintf("\n... %d bytes omitted ...\n", stored.Size-budget) + string(bytesToValidUTF8(tail))
		truncated = true
	}
	if stored.OmittedBytes > 0 {
		text = fmt.Sprintf("... first %d bytes of output were lost ...\n", stored.OmittedBytes) + text
	}
	return text, truncated, nil
}

func setEnvironment(environment []string, name, value string) {
	prefix := name + "="
	for i := range environment {
		if strings.HasPrefix(environment[i], prefix) {
			environment[i] = prefix + value
			return
		}
	}
}

func executableFile(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("not an executable regular file")
	}
	return resolved, nil
}

func (r *Runner) readStoredOutput(stored StoredOutput) (string, bool, error) {
	return r.readStoredOutputWithBudget(stored, r.config.MaxOutputBytes)
}

func (r *Runner) terminate(pid int, wait <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(r.config.TerminationGrace)
	defer timer.Stop()
	var waitErr error
	select {
	case err := <-wait:
		waitErr = err
		// The shell can exit before descendants which ignored SIGTERM. Keep
		// supervising the process group through the grace period.
		for groupExists(pid) {
			select {
			case <-timer.C:
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				return waitErr
			case <-time.After(10 * time.Millisecond):
			}
		}
		return waitErr
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-wait
	}
}

func groupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (r *Runner) environment(overrides map[string]string) ([]string, error) {
	values := map[string]string{"TMPDIR": "/tmp"}
	for _, name := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TERM", "TZ"} {
		if value, ok := os.LookupEnv(name); ok && !unsafeEnvironmentName(name) {
			values[name] = value
		}
	}
	for name, value := range r.config.Environment {
		if strings.IndexByte(name, '=') >= 0 || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("process: invalid environment value")
		}
		if unsafeEnvironmentName(name) && !r.config.AllowUnsafeEnvironment {
			continue
		}
		values[name] = value
	}
	for name, value := range overrides {
		if strings.IndexByte(name, '=') >= 0 || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("process: invalid environment value")
		}
		if unsafeEnvironmentName(name) && !r.config.AllowUnsafeEnvironment {
			return nil, fmt.Errorf("process: unsafe environment variable %s is disabled", name)
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result, nil
}

func unsafeEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "DYLD_") || upper == "LD_PRELOAD" || upper == "LD_LIBRARY_PATH" {
		return true
	}
	switch upper {
	case "BASH_ENV", "ENV", "ZDOTDIR", "PYTHONSTARTUP", "PYTHONPATH", "PERL5OPT", "RUBYOPT", "NODE_OPTIONS", "PROMPT_COMMAND", "CDPATH":
		return true
	default:
		return false
	}
}

// buildProfile constructs a concrete security.SecurityProfile for a sandboxed
// command by combining the request's profile with session-enriched rules.
func (r *Runner) buildProfile(profile security.SecurityProfile, sessionID, cwd string) (security.SecurityProfile, error) {
	workspaceRoot := r.config.Workspace.Root()

	var profileRules []security.Rule
	var readOnly bool
	if profile != nil {
		profileRules = profile.Rules()
		readOnly = profile.IsReadOnly()
	}

	rules := make([]security.Rule, 0, len(profileRules)+len(r.config.SandboxRules)+8)
	if !readOnly {
		writePaths := []string{workspaceRoot}
		granted, err := r.writableForSession(sessionID)
		if err != nil {
			return nil, err
		}
		writePaths = append(writePaths, granted...)
		if commonDir, ok := linkedGitCommonDirectory(workspaceRoot); ok {
			writePaths = append(writePaths, commonDir)
		}
		for _, path := range writePaths {
			rules = append(rules, security.Rule{Path: path, Action: security.ActionAllowWrite})
		}
	}
	rules = append(rules, profileRules...)
	for _, path := range protectedWorkspacePaths(workspaceRoot, cwd) {
		rules = append(rules, security.Rule{Path: path, Action: security.ActionDenyWrite})
	}

	configuredRules := r.config.SandboxRules
	if readOnly {
		configuredRules = rulesForReadOnlyProfile(configuredRules, allowWriteRulePaths(profileRules))
	}
	rules = append(rules, configuredRules...)
	return &sandboxProfile{readOnly: readOnly, rules: rules}, nil
}

func allowWriteRulePaths(rules []security.Rule) []string {
	paths := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule.Action == security.ActionAllowWrite {
			paths = append(paths, rule.Path)
		}
	}
	return paths
}

// rulesForReadOnlyProfile keeps configured rules from widening a read-only
// profile or masking one of its deliberately writable paths. Other read
// restrictions still apply, and writable profiles retain the configured rule
// ordering unchanged.
func rulesForReadOnlyProfile(rules []security.Rule, writePaths []string) []security.Rule {
	filtered := make([]security.Rule, 0, len(rules))
	for _, rule := range rules {
		if rule.Action == security.ActionAllowWrite || overlapsAnyPath(rule.Path, writePaths) {
			continue
		}
		filtered = append(filtered, rule)
	}
	return filtered
}

func overlapsAnyPath(path string, paths []string) bool {
	path = filepath.Clean(path)
	for _, candidate := range paths {
		candidate = filepath.Clean(candidate)
		if isPathWithin(path, candidate) || isPathWithin(candidate, path) {
			return true
		}
	}
	return false
}

// streamWriter fans command output out to managed storage and the live
// stream. Storage failures are recorded and reported once the command
// finishes so the child process is never blocked by a broken sink.
type streamWriter struct {
	mu     sync.Mutex
	output io.Writer
	stream io.Writer
	tail   lineTail
	err    error
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail.Write(p)
	if w.err == nil {
		if _, err := w.output.Write(p); err != nil {
			w.err = err
		}
	}
	if w.stream != nil {
		if _, err := w.stream.Write(p); err != nil {
			w.stream = nil
		}
	}
	return len(p), nil
}

const (
	outputTailLines     = 10
	outputTailLineBytes = 16 << 10
)

type lineTail struct {
	lines    []string
	pending  []byte
	carriage bool
}

func (t *lineTail) Write(value []byte) {
	for _, char := range value {
		if t.carriage {
			t.carriage = false
			if char != '\n' {
				t.pending = t.pending[:0]
			}
		}
		switch char {
		case '\n':
			t.lines = append(t.lines, string(t.pending))
			t.pending = t.pending[:0]
		case '\r':
			t.carriage = true
		default:
			t.pending = append(t.pending, char)
			if len(t.pending) > outputTailLineBytes {
				start := len(t.pending) - outputTailLineBytes
				for start < len(t.pending) && !utf8.RuneStart(t.pending[start]) {
					start++
				}
				t.pending = t.pending[start:]
			}
		}
	}
	if len(t.lines) > outputTailLines {
		t.lines = t.lines[len(t.lines)-outputTailLines:]
	}
}

func (t lineTail) String() string {
	lines := append([]string(nil), t.lines...)
	if len(t.pending) != 0 {
		lines = append(lines, string(t.pending))
	}
	if len(lines) > outputTailLines {
		lines = lines[len(lines)-outputTailLines:]
	}
	return strings.Join(lines, "\n")
}
