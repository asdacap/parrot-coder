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

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type Config struct {
	Workspace              *workspace.Workspace
	Environment            map[string]string
	MaxOutputBytes         int64
	Timeout                time.Duration
	TerminationGrace       time.Duration
	Output                 io.Writer
	OutputStore            OutputStore
	AllowUnsafeEnvironment bool
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
	Shell   string            `json:"shell"`
	Command string            `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

type Result struct {
	Output     string `json:"output"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Cancelled  bool   `json:"cancelled"`
	Truncated  bool   `json:"truncated"`
	OutputID   string `json:"output_id,omitempty"`
	OutputSize int64  `json:"output_size,omitempty"`
}

type Runner struct{ config Config }

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
	return &Runner{config: config}, nil
}

// Run treats the command as arbitrary process execution. The shell path is an
// executable, not a policy boundary, and must be explicitly supplied.
func (r *Runner) Run(ctx context.Context, request Request) (Result, error) {
	if request.Shell == "" || !filepath.IsAbs(request.Shell) || request.Command == "" {
		return Result{}, errors.New("process: absolute shell path and command are required")
	}
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1, Cancelled: true}, err
	}
	cwd := request.Cwd
	if cwd == "" {
		cwd = "."
	}
	resolved, err := r.config.Workspace.ResolveRead(cwd)
	if err != nil {
		return Result{}, fmt.Errorf("process: resolve cwd: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return Result{}, errors.New("process: cwd is not a workspace directory")
	}
	environment, err := r.environment(request.Env)
	if err != nil {
		return Result{}, err
	}

	capture := &boundedWriter{limit: r.config.MaxOutputBytes, output: r.config.Output}
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
	command := exec.Command(request.Shell, "-c", request.Command)
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
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return result, fmt.Errorf("process: wait: %w", waitErr)
	}
	return result, nil
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
	values := map[string]string{}
	for _, name := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TERM", "TMPDIR", "TZ"} {
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
	truncated bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	if int64(len(p)) > remaining {
		w.truncated = true
	}
	return len(p), nil
}

func (w *boundedWriter) result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.data...)), w.truncated
}
