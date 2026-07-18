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

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type Config struct {
	Workspace              *workspace.Workspace
	WorkingDirectory       string
	Environment            map[string]string
	MaxOutputBytes         int64
	Timeout                time.Duration
	TerminationGrace       time.Duration
	Output                 io.Writer
	OutputStore            OutputStore
	AllowUnsafeEnvironment bool
	MaxProcesses           int
	MaxSessionProcesses    int
	sandbox                sandbox
}

type StoredOutput struct {
	ID      string
	Size    int64
	Preview string
}

type OutputStore interface {
	Store(context.Context, io.Reader) (StoredOutput, error)
}

type Request struct {
	Shell     string            `json:"shell"`
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Timeout   time.Duration     `json:"timeout,omitempty"`
	Output    io.Writer         `json:"-"`
	SessionID string            `json:"-"`
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
	config        Config
	sandbox       sandbox
	mu            sync.RWMutex
	writablePaths map[string]map[string]struct{}
	processes     map[int32]*persistentProcess
	reservedIDs   map[int32]string
	closed        bool
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
		config.MaxOutputBytes = 1 << 20
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
	return &Runner{
		config: config, sandbox: implementation,
		writablePaths: make(map[string]map[string]struct{}),
		processes:     make(map[int32]*persistentProcess), reservedIDs: make(map[int32]string),
	}, nil
}

// WorkingDirectory returns the canonical default cwd used by process tools.
func (r *Runner) WorkingDirectory() string { return r.config.WorkingDirectory }

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

	capture := &boundedWriter{limit: r.config.MaxOutputBytes, output: r.config.Output, stream: request.Output}
	var outputPipe *io.PipeWriter
	var stored <-chan storedResult
	if r.config.OutputStore != nil {
		reader, writer := io.Pipe()
		outputPipe = writer
		capture.output = writer
		result := make(chan storedResult, 1)
		stored = result
		go func() {
			output, storeErr := r.config.OutputStore.Store(ctx, reader)
			_ = reader.CloseWithError(storeErr)
			result <- storedResult{output: output, err: storeErr}
		}()
	}
	resolvedShell, err := executableFile(request.Shell)
	if err != nil {
		if outputPipe != nil {
			_ = outputPipe.CloseWithError(err)
			<-stored
		}
		return Result{}, fmt.Errorf("process: shell: %w", err)
	}
	program, arguments := resolvedShell, []string{"-c", request.Command}
	if sandboxed {
		writablePaths, writableErr := r.writableForSession(request.SessionID)
		if writableErr != nil {
			if outputPipe != nil {
				_ = outputPipe.CloseWithError(writableErr)
				<-stored
			}
			return Result{}, writableErr
		}
		program, arguments, err = r.sandbox.command(resolvedShell, request.Command, resolved, writablePaths)
		if err != nil {
			if outputPipe != nil {
				_ = outputPipe.CloseWithError(err)
				<-stored
			}
			return Result{}, fmt.Errorf("process: sandbox: %w", err)
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
		if outputPipe != nil {
			_ = outputPipe.CloseWithError(err)
			<-stored
		}
		return Result{}, fmt.Errorf("process: start: %w", err)
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
	if outputPipe != nil {
		_ = outputPipe.Close()
		storedResult := <-stored
		if storedResult.err == nil {
			result.OutputID = storedResult.output.ID
			result.OutputSize = storedResult.output.Size
		}
	}
	result.Output, result.Truncated = capture.result()
	result.OutputTail = capture.tail.String()
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return result, fmt.Errorf("process: wait: %w", waitErr)
	}
	return result, nil
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

type storedResult struct {
	output StoredOutput
	err    error
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

type boundedWriter struct {
	mu        sync.Mutex
	data      []byte
	limit     int64
	output    io.Writer
	stream    io.Writer
	truncated bool
	tail      lineTail
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail.Write(p)
	remaining := w.limit - int64(len(w.data))
	if remaining > 0 {
		keep := int64(len(p))
		if keep > remaining {
			keep = remaining
		}
		w.data = append(w.data, p[:keep]...)
	}
	if w.output != nil {
		if _, err := w.output.Write(p); err != nil {
			// Managed output is best-effort. Keep the bounded in-memory result
			// and let the process continue if storage reaches its quota.
			w.output = nil
		}
	}
	if w.stream != nil {
		if _, err := w.stream.Write(p); err != nil {
			w.stream = nil
		}
	}
	if int64(len(p)) > remaining {
		w.truncated = true
	}
	return len(p), nil
}

const (
	outputTailLines     = 3
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

func (w *boundedWriter) result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.data...)), w.truncated
}
