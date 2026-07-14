// Package diagnostics records process lifecycle information and recovered
// panics in Parrot's private state directory.
package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	directoryName = "diagnostics"
	logName       = "parrot.jsonl"
	crashName     = "crash.log"
	maxLogSize    = 4 << 20
	logBackups    = 3
)

// Build describes the executable that started a run.
type Build struct {
	Version string
	Commit  string
	Date    string
}

// Run owns the durable files for one process invocation.
type Run struct {
	mu        sync.Mutex
	file      *os.File
	crashFile *os.File
	logPath   string
	logSize   int64
	marker    string
	runID     string
	pid       int
	started   time.Time
	closed    bool
}

type marker struct {
	RunID     string    `json:"run_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

var global struct {
	sync.RWMutex
	run *Run
}

// Start opens the process log, configures Go's runtime crash-output duplicate,
// reports stale run markers, and creates a marker for this invocation.
func Start(stateDir string, build Build) (*Run, error) {
	dir := filepath.Join(stateDir, directoryName)
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("diagnostics directory must be absolute: %q", dir)
	}
	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostics directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure diagnostics directory: %w", err)
	}
	if err := os.Chmod(runsDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure diagnostics run directory: %w", err)
	}
	if err := rotate(filepath.Join(dir, logName)); err != nil {
		return nil, err
	}
	if err := rotate(filepath.Join(dir, crashName)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, logName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open diagnostics log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure diagnostics log: %w", err)
	}
	crashFile, err := os.OpenFile(filepath.Join(dir, crashName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("open crash log: %w", err)
	}
	if err := crashFile.Chmod(0o600); err != nil {
		_ = crashFile.Close()
		_ = file.Close()
		return nil, fmt.Errorf("secure crash log: %w", err)
	}
	if err := debug.SetCrashOutput(crashFile, debug.CrashOptions{}); err != nil {
		_ = crashFile.Close()
		_ = file.Close()
		return nil, fmt.Errorf("configure runtime crash log: %w", err)
	}

	logInfo, err := file.Stat()
	if err != nil {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = crashFile.Close()
		_ = file.Close()
		return nil, fmt.Errorf("inspect open diagnostics log: %w", err)
	}
	now := time.Now().UTC()
	r := &Run{
		file: file, crashFile: crashFile, logPath: filepath.Join(dir, logName), logSize: logInfo.Size(),
		runID: newRunID(), pid: os.Getpid(), started: now,
	}
	global.Lock()
	global.run = r
	global.Unlock()
	r.reportStaleMarkers(runsDir)
	r.marker = filepath.Join(runsDir, strconv.Itoa(r.pid)+"-"+r.runID+".json")
	if err := writeMarker(r.marker, marker{RunID: r.runID, PID: r.pid, StartedAt: now}); err != nil {
		r.log("error", "run_marker_write_failed", true, "error", err.Error())
	}
	cwd, _ := os.Getwd()
	executable, _ := os.Executable()
	r.log("info", "process_started", true,
		"version", build.Version, "commit", build.Commit, "built_at", build.Date,
		"go_version", readBuildInfoVersion(), "go_os", runtime.GOOS, "go_arch", runtime.GOARCH,
		"working_directory", cwd, "executable", executable,
	)
	return r, nil
}

// Event appends an informational event if diagnostics were initialized.
func Event(name string, attributes ...any) {
	global.RLock()
	r := global.run
	global.RUnlock()
	if r != nil {
		r.log("info", name, false, attributes...)
	}
}

// Warn appends a warning event if diagnostics were initialized.
func Warn(name string, attributes ...any) {
	global.RLock()
	r := global.run
	global.RUnlock()
	if r != nil {
		r.log("warn", name, false, attributes...)
	}
}

// Error appends a non-synchronized error event if diagnostics were initialized.
// Use Critical when the record must be flushed before returning.
func Error(name string, attributes ...any) {
	global.RLock()
	r := global.run
	global.RUnlock()
	if r != nil {
		r.log("error", name, false, attributes...)
	}
}

// Critical appends and synchronizes an event before returning.
func Critical(name string, attributes ...any) {
	global.RLock()
	r := global.run
	global.RUnlock()
	if r != nil {
		r.log("error", name, true, attributes...)
	}
}

// Panic records a recovered panic and the complete current goroutine stack.
func Panic(component string, recovered any) {
	PanicWithStack(component, recovered, debug.Stack())
}

// PanicWithStack records a recovered panic with a stack captured at its
// recovery boundary. Additional attributes may identify the failed operation,
// but callers must not include prompts, tool inputs, or other private content.
func PanicWithStack(component string, recovered any, stack []byte, attributes ...any) {
	attributes = append([]any{
		"component", component,
		"panic_type", fmt.Sprintf("%T", recovered),
		"panic", formatPanic(recovered),
		"stack", boundedString(string(stack), 1<<20),
	}, attributes...)
	Critical("panic_recovered",
		attributes...,
	)
}

// ErrorType returns a content-free error classification suitable for logs.
// It intentionally never includes Error(), which may contain prompts, provider
// responses, paths, command arguments, or credentials.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	return fmt.Sprintf("%T", err)
}

func formatPanic(recovered any) (text string) {
	defer func() {
		if recover() != nil {
			text = "<panic value formatting failed>"
		}
	}()
	return boundedString(fmt.Sprint(recovered), 16<<10)
}

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n...[truncated]"
}

// Finish records an orderly process exit and removes its run marker.
func (r *Run) Finish(exitCode int) {
	if r == nil {
		return
	}
	r.log("info", "process_exited", true, "exit_code", exitCode, "runtime_ms", time.Since(r.started).Milliseconds())
	if r.marker != "" {
		if err := os.Remove(r.marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.log("error", "run_marker_remove_failed", true, "error", err.Error())
		}
	}
	global.Lock()
	if global.run == r {
		global.run = nil
	}
	global.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	_ = r.crashFile.Close()
	_ = r.file.Close()
}

func (r *Run) reportStaleMarkers(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		r.log("error", "run_marker_scan_failed", false, "error", err.Error())
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, readErr := os.ReadFile(path)
		var previous marker
		parseErr := json.Unmarshal(data, &previous)
		if readErr == nil && parseErr == nil && previous.PID > 0 && previous.PID != r.pid && processAlive(previous.PID) {
			continue
		}
		attributes := []any{"marker", entry.Name()}
		if previous.RunID != "" {
			attributes = append(attributes, "previous_run_id", previous.RunID, "previous_pid", previous.PID, "previous_started_at", previous.StartedAt)
		}
		if readErr != nil {
			attributes = append(attributes, "marker_error", readErr.Error())
		} else if parseErr != nil {
			attributes = append(attributes, "marker_error", parseErr.Error())
		}
		r.log("warn", "unclean_previous_exit", true, attributes...)
		_ = os.Remove(path)
	}
}

func (r *Run) log(level, event string, syncFile bool, attributes ...any) {
	if r == nil {
		return
	}
	record := map[string]any{
		"time":   time.Now().UTC().Format(time.RFC3339Nano),
		"level":  level,
		"event":  event,
		"pid":    r.pid,
		"run_id": r.runID,
	}
	for index := 0; index+1 < len(attributes); index += 2 {
		key, ok := attributes[index].(string)
		if !ok || key == "" {
			continue
		}
		record[key] = attributes[index+1]
	}
	data, err := json.Marshal(record)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"time":%q,"level":"error","event":"diagnostics_encode_failed","error":%q}`, time.Now().UTC().Format(time.RFC3339Nano), err.Error()))
	}
	data = append(data, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.file == nil {
		return
	}
	if r.logSize > 0 && r.logSize+int64(len(data)) > maxLogSize {
		if err := r.rotateLogLocked(); err != nil {
			return
		}
	}
	written, _ := r.file.Write(data)
	r.logSize += int64(written)
	if syncFile {
		_ = r.file.Sync()
	}
}

func (r *Run) rotateLogLocked() error {
	if err := r.file.Sync(); err != nil {
		return err
	}
	if err := r.file.Close(); err != nil {
		return err
	}
	r.file = nil
	if err := rotateExisting(r.logPath); err != nil {
		return err
	}
	file, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	r.file = file
	r.logSize = 0
	return nil
}

func writeMarker(path string, value marker) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

func rotate(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Size() < maxLogSize {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect diagnostics log: %w", err)
	}
	return rotateExisting(path)
}

func rotateExisting(path string) error {
	_ = os.Remove(path + "." + strconv.Itoa(logBackups))
	for index := logBackups - 1; index >= 1; index-- {
		oldPath := path + "." + strconv.Itoa(index)
		newPath := path + "." + strconv.Itoa(index+1)
		if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate diagnostics log: %w", err)
		}
	}
	if err := os.Rename(path, path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate diagnostics log: %w", err)
	}
	return nil
}

func newRunID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func readBuildInfoVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}
