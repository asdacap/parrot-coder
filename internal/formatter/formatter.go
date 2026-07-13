// Package formatter runs configured formatters and returns reviewable proposals.
package formatter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Mode string

const (
	ModeStdin Mode = "stdin"
	ModeFile  Mode = "file"
)

var (
	ErrStale       = errors.New("formatter: file hash is stale")
	ErrOutputLimit = errors.New("formatter: output limit exceeded")
)

type Formatter struct {
	Name       string
	Extensions []string
	Command    []string
	Mode       Mode
}

type Config struct {
	Workspace        string
	Environment      map[string]string
	Timeout          time.Duration
	MaxOutputBytes   int64
	TerminationGrace time.Duration
}

type Registry struct {
	config     Config
	mu         sync.RWMutex
	formatters []Formatter
}

type Review struct {
	Path         string   `json:"path"`
	ExpectedHash string   `json:"expected_hash"`
	Formatter    string   `json:"formatter"`
	Command      []string `json:"command"`
}

type Plan struct {
	Path         string
	ExpectedHash string
	Formatter    string
	Command      []string
	Mode         Mode
	Review       Review
}

type Result struct {
	Path       string
	BeforeHash string
	AfterHash  string
	Proposed   []byte
	Diff       string
	Changed    bool
	Warning    string
}

func NewRegistry(config Config, formatters ...Formatter) (*Registry, error) {
	if config.Workspace == "" || !filepath.IsAbs(config.Workspace) {
		return nil, errors.New("formatter: workspace must be absolute")
	}
	root, err := filepath.EvalSymlinks(config.Workspace)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("formatter: workspace is not a directory")
	}
	config.Workspace = root
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = 4 << 20
	}
	if config.TerminationGrace <= 0 {
		config.TerminationGrace = 250 * time.Millisecond
	}
	r := &Registry{config: config}
	for _, formatter := range formatters {
		if err := r.Register(formatter); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds a formatter. Selection remains deterministic regardless of registration order.
func (r *Registry) Register(formatter Formatter) error {
	if formatter.Name == "" || len(formatter.Command) == 0 || formatter.Command[0] == "" || !filepath.IsAbs(formatter.Command[0]) {
		return errors.New("formatter: name and absolute command are required")
	}
	if formatter.Mode == "" {
		formatter.Mode = ModeStdin
	}
	if formatter.Mode != ModeStdin && formatter.Mode != ModeFile {
		return errors.New("formatter: invalid mode")
	}
	if len(formatter.Extensions) == 0 {
		return errors.New("formatter: extension is required")
	}
	if formatter.Mode == ModeFile && !containsPlaceholder(formatter.Command) {
		return errors.New("formatter: file mode command requires {file}")
	}
	for i, extension := range formatter.Extensions {
		if extension == "" {
			return errors.New("formatter: empty extension")
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		formatter.Extensions[i] = strings.ToLower(extension)
	}
	formatter.Command = append([]string(nil), formatter.Command...)
	formatter.Extensions = append([]string(nil), formatter.Extensions...)
	r.mu.Lock()
	r.formatters = append(r.formatters, formatter)
	r.mu.Unlock()
	return nil
}

// Hash returns the lowercase SHA-256 hash used by Plan and Format.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Plan validates the current file hash and creates a deterministic review command.
func (r *Registry) Plan(path, expectedHash string) (Plan, error) {
	resolved, data, err := r.readWorkspaceFile(path)
	if err != nil {
		return Plan{}, err
	}
	if expectedHash == "" || !strings.EqualFold(Hash(data), expectedHash) {
		return Plan{}, ErrStale
	}
	formatter, err := r.selectFormatter(filepath.Ext(resolved))
	if err != nil {
		return Plan{}, err
	}
	command := expandFile(formatter.Command, resolved)
	plan := Plan{
		Path:         resolved,
		ExpectedHash: strings.ToLower(expectedHash),
		Formatter:    formatter.Name,
		Command:      command,
		Mode:         formatter.Mode,
	}
	plan.Review = Review{Path: plan.Path, ExpectedHash: plan.ExpectedHash, Formatter: plan.Formatter, Command: append([]string(nil), command...)}
	return plan, nil
}

func (r *Registry) selectFormatter(extension string) (Formatter, error) {
	extension = strings.ToLower(extension)
	r.mu.RLock()
	var matches []Formatter
	for _, formatter := range r.formatters {
		for _, candidate := range formatter.Extensions {
			if candidate == extension {
				matches = append(matches, formatter)
				break
			}
		}
	}
	r.mu.RUnlock()
	if len(matches) == 0 {
		return Formatter{}, errors.New("formatter: no formatter for extension")
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Name != matches[j].Name {
			return matches[i].Name < matches[j].Name
		}
		return strings.Join(matches[i].Command, "\x00") < strings.Join(matches[j].Command, "\x00")
	})
	return matches[0], nil
}

// Format revalidates the reviewed hash and returns bytes and a diff without writing the file.
func (r *Registry) Format(ctx context.Context, plan Plan) (Result, error) {
	resolved, before, err := r.readWorkspaceFile(plan.Path)
	if err != nil {
		return Result{}, err
	}
	beforeHash := Hash(before)
	if !strings.EqualFold(beforeHash, plan.ExpectedHash) {
		return Result{}, ErrStale
	}
	formatter, err := r.selectFormatter(filepath.Ext(resolved))
	if err != nil {
		return Result{}, err
	}
	if formatter.Name != plan.Formatter || formatter.Mode != plan.Mode || !equalStrings(expandFile(formatter.Command, resolved), plan.Command) {
		return Result{}, errors.New("formatter: plan does not match registry")
	}

	temp, err := os.CreateTemp("", "parrot-format-*"+filepath.Ext(resolved))
	if err != nil {
		return Result{}, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(before); err != nil {
		temp.Close()
		return Result{}, err
	}
	if err := temp.Close(); err != nil {
		return Result{}, err
	}

	command := expandFile(formatter.Command, tempPath)
	var stdin io.Reader
	if formatter.Mode == ModeStdin {
		stdin = bytes.NewReader(before)
	}
	stdout, stderr, err := r.run(ctx, command, stdin)
	if err != nil {
		if len(stderr) != 0 {
			return Result{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return Result{}, err
	}
	proposed := stdout
	if formatter.Mode == ModeFile {
		proposed, err = readBoundedFile(tempPath, r.config.MaxOutputBytes)
		if err != nil {
			return Result{}, err
		}
	}
	result := Result{
		Path:       resolved,
		BeforeHash: beforeHash,
		AfterHash:  Hash(proposed),
		Proposed:   append([]byte(nil), proposed...),
		Changed:    !bytes.Equal(before, proposed),
	}
	if result.Changed {
		result.Diff = unifiedDiff(resolved, before, proposed)
	}
	return result, nil
}

// Pipeline is intended for post-mutation hooks. Formatting failures are warnings,
// leaving the caller's mutation and file contents untouched.
func (r *Registry) Pipeline(ctx context.Context, path, expectedHash string) Result {
	plan, err := r.Plan(path, expectedHash)
	if err != nil {
		return Result{Path: path, BeforeHash: expectedHash, Warning: err.Error()}
	}
	result, err := r.Format(ctx, plan)
	if err != nil {
		return Result{Path: plan.Path, BeforeHash: expectedHash, Warning: err.Error()}
	}
	return result
}

func (r *Registry) FormatAfterMutation(ctx context.Context, path, expectedHash string) Result {
	return r.Pipeline(ctx, path, expectedHash)
}

func (r *Registry) run(ctx context.Context, argv []string, stdin io.Reader) ([]byte, []byte, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("formatter: empty command")
	}
	runCtx, cancel := context.WithTimeout(ctx, r.config.Timeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = r.config.Workspace
	cmd.Env = formatterEnvironment(r.config.Environment)
	cmd.Stdin = stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := &limitBuffer{max: r.config.MaxOutputBytes}
	stderr := &limitBuffer{max: min(r.config.MaxOutputBytes, 64<<10)}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("formatter: start: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-runCtx.Done():
		waitErr = r.stop(cmd.Process.Pid, wait)
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("formatter: timeout: %w", runCtx.Err())
		}
		return stdout.Bytes(), stderr.Bytes(), runCtx.Err()
	}
	if stdout.exceeded.Load() || stderr.exceeded.Load() {
		return stdout.Bytes(), stderr.Bytes(), ErrOutputLimit
	}
	if waitErr != nil {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("formatter: command failed: %w", waitErr)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func (r *Registry) stop(pid int, wait <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(r.config.TerminationGrace)
	defer timer.Stop()
	select {
	case err := <-wait:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return err
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-wait
	}
}

type limitBuffer struct {
	buffer   bytes.Buffer
	max      int64
	exceeded atomic.Bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	remaining := b.max - int64(b.buffer.Len())
	if remaining <= 0 {
		b.exceeded.Store(true)
		return 0, ErrOutputLimit
	}
	if int64(len(p)) > remaining {
		b.buffer.Write(p[:remaining])
		b.exceeded.Store(true)
		return int(remaining), ErrOutputLimit
	}
	return b.buffer.Write(p)
}

func (b *limitBuffer) Bytes() []byte { return append([]byte(nil), b.buffer.Bytes()...) }

func (r *Registry) readWorkspaceFile(path string) (string, []byte, error) {
	resolved, err := r.resolve(path)
	if err != nil {
		return "", nil, err
	}
	data, err := readBoundedFile(resolved, r.config.MaxOutputBytes)
	if err != nil {
		return "", nil, err
	}
	return resolved, data, nil
}

func (r *Registry) resolve(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("formatter: invalid path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.config.Workspace, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.config.Workspace, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("formatter: path is outside workspace")
	}
	return filepath.Clean(resolved), nil
}

func containsPlaceholder(command []string) bool {
	for _, arg := range command {
		if strings.Contains(arg, "{file}") {
			return true
		}
	}
	return false
}

func expandFile(command []string, path string) []string {
	result := make([]string, len(command))
	for i, arg := range command {
		result[i] = strings.ReplaceAll(arg, "{file}", path)
	}
	return result
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
