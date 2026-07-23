package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	pathAdviceTimeout       = time.Second
	pathAdviceMaxVisited    = 100000
	pathAdviceMaxFiles      = 8
	pathAdviceMaxReferences = 8
	pathAdviceMaxBytes      = 8 << 10
	pathAdviceMaxScanBytes  = 64 << 20
)

// ErrorAdvicePath describes a path involved in a failed invocation and any
// exact existing content which could identify where that path moved.
type ErrorAdvicePath struct {
	Path          string
	ExactContents []string
}

// ErrorAdvice contains tool-declared context for enriching an error. Declaring
// this context on the tool keeps the executor independent of tool IDs and input
// schemas.
type ErrorAdvice struct {
	Paths []ErrorAdvicePath
}

// ErrorAdviceProvider is an optional tool capability.
type ErrorAdviceProvider interface {
	ErrorAdvice(json.RawMessage) (ErrorAdvice, error)
}

// ErrorAdvisor may add actionable context to an error before it is returned to
// the agent. Implementations must preserve the original error's identity.
type ErrorAdvisor interface {
	Advise(context.Context, error, ErrorAdvice) error
}

// PathContentSearcher locates fixed strings in text files beneath root.
type PathContentSearcher interface {
	Search(context.Context, string, []string) (string, bool, error)
}

// PathErrorAdvisor suggests workspace files and references when a tool was
// given a path which does not exist.
type PathErrorAdvisor struct {
	root     string
	searcher PathContentSearcher
	timeout  time.Duration
}

func NewPathErrorAdvisor(root string, searcher PathContentSearcher) *PathErrorAdvisor {
	return &PathErrorAdvisor{root: root, searcher: searcher, timeout: pathAdviceTimeout}
}

func (a *PathErrorAdvisor) Advise(ctx context.Context, original error, details ErrorAdvice) error {
	if a == nil || original == nil || !errors.Is(original, os.ErrNotExist) || a.root == "" {
		return original
	}
	var pathError *os.PathError
	if !errors.As(original, &pathError) || !errors.Is(pathError, os.ErrNotExist) {
		return original
	}
	candidate, ok := matchingAdvicePath(details.Paths, a.root, pathError.Path)
	if !ok {
		return original
	}
	basename := filepath.Base(candidate.Path)
	if basename == "" || basename == "." || basename == string(filepath.Separator) {
		return original
	}

	timeout := a.timeout
	if timeout <= 0 {
		timeout = pathAdviceTimeout
	}
	searchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	files, filesTruncated := findWorkspaceBasenames(searchCtx, a.root, basename)
	exactMatches, exactTruncated := findWorkspaceContents(searchCtx, a.root, candidate.ExactContents)
	var references []string
	referencesTruncated := false
	if a.searcher != nil && searchCtx.Err() == nil {
		queries := []string{basename}
		if stem := strings.TrimSuffix(basename, filepath.Ext(basename)); len(stem) >= 3 && stem != basename {
			queries = append(queries, stem)
		}
		if output, truncated, err := a.searcher.Search(searchCtx, a.root, queries); err == nil {
			references, referencesTruncated = boundedReferenceLines(output, pathAdviceMaxReferences)
			referencesTruncated = referencesTruncated || truncated
		}
	}
	if len(files) == 0 && len(exactMatches) == 0 && len(references) == 0 {
		return original
	}

	var advice strings.Builder
	if len(files) > 0 {
		fmt.Fprintf(&advice, "Files named %q exist at:", basename)
		for _, path := range files {
			fmt.Fprintf(&advice, "\n- %s", path)
		}
		if filesTruncated {
			advice.WriteString("\n- ... more filename matches omitted")
		}
	}
	if len(exactMatches) > 0 {
		if advice.Len() > 0 {
			advice.WriteByte('\n')
		}
		advice.WriteString("Files containing the exact requested text:")
		for _, path := range exactMatches {
			fmt.Fprintf(&advice, "\n- %s", path)
		}
		if exactTruncated {
			advice.WriteString("\n- ... more exact text matches omitted")
		}
	}
	if len(references) > 0 {
		if advice.Len() > 0 {
			advice.WriteByte('\n')
		}
		fmt.Fprintf(&advice, "%q (or its filename stem) appears at:", basename)
		for _, reference := range references {
			fmt.Fprintf(&advice, "\n- %s", reference)
		}
		if referencesTruncated {
			advice.WriteString("\n- ... more text matches omitted")
		}
	}
	return advisedError{err: original, advice: advice.String()}
}

type advisedError struct {
	err    error
	advice string
}

func (e advisedError) Error() string { return e.err.Error() + "\n\nAdvisor:\n" + e.advice }
func (e advisedError) Unwrap() error { return e.err }

var errPathAdviceLimit = errors.New("path advice search limit reached")

func findWorkspaceBasenames(ctx context.Context, root, basename string) ([]string, bool) {
	var matches []string
	visited := 0
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path != root && entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		visited++
		if visited > pathAdviceMaxVisited {
			truncated = true
			return errPathAdviceLimit
		}
		if path == root || entry.IsDir() || entry.Name() != basename {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if len(matches) == pathAdviceMaxFiles {
			truncated = true
			return errPathAdviceLimit
		}
		matches = append(matches, filepath.ToSlash(relative))
		return nil
	})
	if err != nil && !errors.Is(err, errPathAdviceLimit) {
		return matches, truncated
	}
	sort.Strings(matches)
	return matches, truncated
}

func matchingAdvicePath(paths []ErrorAdvicePath, root, missingPath string) (ErrorAdvicePath, bool) {
	missingPath = absoluteAdvicePath(root, missingPath)
	for _, candidate := range paths {
		candidate.Path = strings.TrimSpace(candidate.Path)
		if candidate.Path == "" || strings.ContainsRune(candidate.Path, '\x00') {
			continue
		}
		absolute := absoluteAdvicePath(root, candidate.Path)
		if absolute == missingPath || strings.HasPrefix(absolute, missingPath+string(filepath.Separator)) {
			return candidate, true
		}
	}
	return ErrorAdvicePath{}, false
}

func absoluteAdvicePath(root, path string) string {
	path = filepath.FromSlash(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return filepath.Clean(path)
}

func findWorkspaceContents(ctx context.Context, root string, contents []string) ([]string, bool) {
	if len(contents) == 0 {
		return nil, false
	}
	needles := make([][]byte, 0, min(len(contents), pathAdviceMaxReferences))
	seen := make(map[string]struct{})
	for _, content := range contents {
		if content == "" {
			continue
		}
		if _, ok := seen[content]; ok {
			continue
		}
		seen[content] = struct{}{}
		needles = append(needles, []byte(content))
		if len(needles) == pathAdviceMaxReferences {
			break
		}
	}
	if len(needles) == 0 {
		return nil, false
	}
	var matches []string
	visited := 0
	var scannedBytes int64
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path != root && entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		visited++
		if visited > pathAdviceMaxVisited {
			truncated = true
			return errPathAdviceLimit
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil
		}
		file := os.NewFile(uintptr(fd), path)
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
			file.Close()
			return nil
		}
		if scannedBytes+info.Size() > pathAdviceMaxScanBytes {
			file.Close()
			truncated = true
			return errPathAdviceLimit
		}
		content, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
		file.Close()
		if err != nil || len(content) > 4<<20 {
			return nil
		}
		scannedBytes += int64(len(content))
		for _, needle := range needles {
			if !bytes.Contains(content, needle) {
				continue
			}
			relative, err := filepath.Rel(root, path)
			if err == nil {
				matches = append(matches, filepath.ToSlash(relative))
			}
			if len(matches) == pathAdviceMaxFiles {
				truncated = true
				return errPathAdviceLimit
			}
			break
		}
		return nil
	})
	if err != nil && !errors.Is(err, errPathAdviceLimit) {
		return matches, truncated
	}
	sort.Strings(matches)
	return matches, truncated
}

func boundedReferenceLines(output string, limit int) ([]string, bool) {
	seen := make(map[string]struct{})
	var lines []string
	truncated := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "./"))
		if line == "" {
			continue
		}
		line = boundedText(line, 512)
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		if len(lines) == limit {
			truncated = true
			continue
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines, truncated
}

// CommandPathContentSearcher uses ripgrep when available and falls back to
// grep. Both programs receive fixed strings as arguments rather than shell
// input, so a basename cannot be interpreted as a regular expression or flag.
type CommandPathContentSearcher struct {
	lookPath func(string) (string, error)
	run      func(context.Context, string, string, []string) (string, bool, error)
}

func NewCommandPathContentSearcher() *CommandPathContentSearcher {
	return &CommandPathContentSearcher{}
}

func (s *CommandPathContentSearcher) Search(ctx context.Context, root string, queries []string) (string, bool, error) {
	lookPath := s.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	run := s.run
	if run == nil {
		run = runPathContentSearch
	}
	var lastErr error
	for _, program := range []string{"rg", "grep"} {
		resolved, err := lookPath(program)
		if err != nil {
			lastErr = err
			continue
		}
		args := pathContentSearchArgs(program, queries)
		output, truncated, err := run(ctx, resolved, root, args)
		if err == nil {
			return output, truncated, nil
		}
		lastErr = err
	}
	return "", false, lastErr
}

func pathContentSearchArgs(program string, queries []string) []string {
	var args []string
	if program == "rg" {
		args = []string{"--line-number", "--fixed-strings", "--no-heading", "--color=never", "--max-count=3"}
	} else {
		args = []string{"-r", "-n", "-F", "-I", "-m", "3", "--exclude-dir=.git"}
	}
	for _, query := range queries {
		args = append(args, "-e", query)
	}
	return append(args, "--", ".")
}

func runPathContentSearch(ctx context.Context, program, root string, args []string) (string, bool, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = root
	var output strings.Builder
	writer := &limitedStringWriter{builder: &output, remaining: pathAdviceMaxBytes}
	command.Stdout = writer
	command.Stderr = &limitedStringWriter{builder: &strings.Builder{}, remaining: 1024}
	err := command.Run()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return output.String(), writer.truncated, nil
}
