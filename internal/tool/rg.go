package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type RgConfig struct {
	MaxFiles     int
	MaxMatches   int
	MaxLineBytes int64
	MaxVisited   int
	Timeout      time.Duration
}
type RgTool struct {
	BasePresentation
	Config  RgConfig
	command rgCommand
}

type rgCommand interface {
	Available() bool
	Search(context.Context, string, []string, rgInput, RgConfig) (Result, error)
}

type cliRgCommand struct {
	path string
}

func NewRgTool(c RgConfig) *RgTool {
	if c.MaxFiles <= 0 {
		c.MaxFiles = 100000
	}
	if c.MaxMatches <= 0 {
		c.MaxMatches = 1000
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = 1 << 20
	}
	if c.MaxVisited <= 0 {
		c.MaxVisited = 1000000
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	path, _ := exec.LookPath("rg")
	return &RgTool{Config: c, command: cliRgCommand{path: path}}
}
func (*RgTool) ID() string { return "rg" }
func (*RgTool) Presentation() Presentation {
	return Presentation{
		Muted: true,
		Label: LabelSpec{Fields: []LabelField{
			{Names: []string{"pattern"}, Quote: true},
			{Names: []string{"path"}, Default: "."},
		}},
	}
}

func (*RgTool) Description() string {
	return "Search text files with regular expressions. Relative paths resolve within the workspace; include optionally filters files with a glob. Uses the rg CLI when available and falls back to an internal Go RE2 search."
}
func (*RgTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input rgInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	path := input.Path
	if path == "" {
		path = "."
	}
	return fmt.Sprintf("Search %q for pattern %q", path, input.Pattern), nil
}
func (*RgTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"include":{"type":"string","description":"Optional glob selecting files to search."}},"required":["pattern"],"additionalProperties":false}`)
}
func (*RgTool) ErrorAdvice(raw json.RawMessage) (ErrorAdvice, error) {
	var input rgInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ErrorAdvice{}, err
	}
	if input.Path == "" {
		input.Path = "."
	}
	return ErrorAdvice{Paths: []ErrorAdvicePath{{Path: input.Path}}}, nil
}

type rgInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Include string `json:"include"`
}
type rgPlan struct {
	Input       rgInput
	Root        string
	Regex       *regexp.Regexp
	Include     *regexp.Regexp
	IncludeBase bool
}

func (p rgPlan) includes(path string) bool {
	if p.Include == nil {
		return true
	}
	candidate := filepath.ToSlash(path)
	if p.IncludeBase {
		candidate = filepath.Base(path)
	}
	return p.Include.MatchString(candidate)
}

type rgLineReader struct {
	ctx           context.Context
	reader        *bufio.Reader
	maxPrefix     int64
	prefix        strings.Builder
	length        int64
	prefixStopped bool
	exists        bool
	done          bool
	err           error
}

func newRgLineReader(ctx context.Context, reader *bufio.Reader, maxPrefix int64) *rgLineReader {
	return &rgLineReader{ctx: ctx, reader: reader, maxPrefix: maxPrefix}
}

func (r *rgLineReader) ReadRune() (rune, int, error) {
	if r.done {
		return 0, 0, io.EOF
	}
	if err := r.ctx.Err(); err != nil {
		r.done = true
		r.err = err
		return 0, 0, err
	}
	value, size, err := r.reader.ReadRune()
	if err != nil {
		r.done = true
		if err != io.EOF {
			r.err = err
		}
		return 0, 0, err
	}
	r.exists = true
	if value == '\n' {
		r.done = true
		return 0, 0, io.EOF
	}
	r.length += int64(size)
	valid := value != utf8.RuneError || size > 1
	if !r.prefixStopped && valid && int64(r.prefix.Len()+size) <= r.maxPrefix {
		r.prefix.WriteRune(value)
	} else if !valid || int64(r.prefix.Len()+size) > r.maxPrefix {
		r.prefixStopped = true
	}
	return value, size, nil
}

func (r *rgLineReader) drain() error {
	for !r.done {
		if _, _, err := r.ReadRune(); err != nil && err != io.EOF {
			break
		}
	}
	return r.err
}

func (r *rgLineReader) truncated() bool { return r.prefixStopped || r.length > r.maxPrefix }

func (t *RgTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("workspace is required")
	}
	var input rgInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	var rx *regexp.Regexp
	var err error
	if t.command == nil || !t.command.Available() {
		rx, err = regexp.Compile(input.Pattern)
		if err != nil {
			return Plan{}, err
		}
	}
	var include *regexp.Regexp
	if input.Include != "" {
		include, err = compileGlob(input.Include)
		if err != nil {
			return Plan{}, err
		}
	}
	path := input.Path
	if path == "" {
		path = "."
	}
	root, err := call.Workspace.ResolveReadOnly(path)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, rgPlan{
		Input:       input,
		Root:        root,
		Regex:       rx,
		Include:     include,
		IncludeBase: !strings.Contains(filepath.ToSlash(input.Include), "/"),
	})
}
func (t *RgTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {

	p := plan.Data.(rgPlan)
	requestedPath := p.Input.Path
	if requestedPath == "" {
		requestedPath = "."
	}
	revalidated, err := call.Workspace.ResolveReadOnly(requestedPath)
	if err != nil {
		return Result{}, err
	}
	if revalidated != p.Root {
		return Result{}, errors.New("rg root changed after planning")
	}
	ctx, cancel := context.WithTimeout(ctx, t.Config.Timeout)
	defer cancel()
	files := []string{}
	info, err := os.Stat(p.Root)
	if err != nil {
		return Result{}, err
	}
	if !info.IsDir() {
		if p.includes(p.Input.Path) {
			files = []string{p.Root}
		}
	} else {
		visited := 0
		err = filepath.WalkDir(p.Root, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			visited++
			if visited > t.Config.MaxVisited {
				return errors.New("rg traversal limit exceeded")
			}
			if d.Type().IsRegular() {
				rel, e := filepath.Rel(p.Root, path)
				if e != nil || !p.includes(rel) {
					return nil
				}
				resolved, e := call.Workspace.ResolveReadOnlyWithin(p.Root, path)
				if e == nil {
					files = append(files, resolved)
					if len(files) > t.Config.MaxFiles {
						return errors.New("rg file limit exceeded")
					}
				}
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
	}
	sort.Strings(files)
	if t.command != nil && t.command.Available() {
		return t.command.Search(ctx, call.Workspace.Root(), files, p.Input, t.Config)
	}
	return t.searchInternal(ctx, files, p, call)
}

func (t *RgTool) searchInternal(ctx context.Context, files []string, p rgPlan, call CallContext) (Result, error) {
	var out strings.Builder
	matches := 0
	truncated := false
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		f, err := os.Open(path)
		if err != nil {
			return Result{}, err
		}
		probe := make([]byte, 8192)
		n, e := f.Read(probe)
		if e != nil && e != io.EOF {
			f.Close()
			return Result{}, e
		}
		if isBinary(probe[:n]) {
			f.Close()
			continue
		}
		_, _ = f.Seek(0, io.SeekStart)
		reader := bufio.NewReader(f)
		lineNo := 0
		for {
			line := newRgLineReader(ctx, reader, t.Config.MaxLineBytes)
			matched := p.Regex.MatchReader(line)
			if err := line.drain(); err != nil {
				f.Close()
				return Result{}, err
			}
			if !line.exists {
				break
			}
			lineNo++
			if matched {
				rel, _ := filepath.Rel(call.Workspace.Root(), path)
				out.WriteString(filepath.ToSlash(rel))
				out.WriteByte(':')
				out.WriteString(strconv.Itoa(lineNo))
				out.WriteByte(':')
				out.WriteString(line.prefix.String())
				if line.truncated() {
					truncated = true
					fmt.Fprintf(&out, "... [truncated; original length: %d bytes]", line.length)
				}
				out.WriteByte('\n')
				matches++
				if matches >= t.Config.MaxMatches {
					f.Close()
					text := out.String()
					return Result{Text: text, ModelText: modelText(text), Metadata: map[string]any{"matches": matches, "truncated": true}}, nil
				}
			}
		}
		f.Close()
	}
	text := out.String()
	metadata := map[string]any{"matches": matches, "files": len(files)}
	if truncated {
		metadata["truncated"] = true
	}
	return Result{Text: text, ModelText: modelText(text), Metadata: metadata}, nil
}

func (c cliRgCommand) Available() bool { return c.path != "" }

const (
	rgBatchFiles = 512
	rgBatchBytes = 64 << 10
)

func (c cliRgCommand) Search(ctx context.Context, workspaceRoot string, files []string, input rgInput, config RgConfig) (Result, error) {
	var out strings.Builder
	matches := 0
	truncated := false
	for start := 0; start < len(files) && matches < config.MaxMatches; {
		end := start
		batchBytes := 0
		for end < len(files) && end-start < rgBatchFiles && (end == start || batchBytes+len(files[end])+1 <= rgBatchBytes) {
			batchBytes += len(files[end]) + 1
			end++
		}
		args := []string{
			"--no-config", "--line-number", "--no-heading", "--with-filename", "--color=never", "--no-messages", "--threads=1",
			"--max-count", strconv.Itoa(config.MaxMatches - matches),
			"--max-columns", strconv.FormatInt(config.MaxLineBytes, 10), "--max-columns-preview",
			"--", input.Pattern,
		}
		args = append(args, files[start:end]...)
		command := exec.CommandContext(ctx, c.path, args...)
		command.Dir = workspaceRoot
		stdout, err := command.StdoutPipe()
		if err != nil {
			return Result{}, fmt.Errorf("rg: %w", err)
		}
		if err := command.Start(); err != nil {
			return Result{}, fmt.Errorf("rg: %w", err)
		}
		reader := bufio.NewReader(stdout)
		reachedLimit := false
		for {
			rawLine, readErr := reader.ReadBytes('\n')
			if len(rawLine) > 0 && rawLine[len(rawLine)-1] == '\n' {
				rawLine = rawLine[:len(rawLine)-1]
			}
			if len(rawLine) > 0 && utf8.Valid(rawLine) {
				line := string(rawLine)
				if strings.HasSuffix(line, " [... omitted end of long line]") || strings.HasSuffix(line, "[Omitted long matching line]") {
					truncated = true
				}
				for _, path := range files[start:end] {
					prefix := path + ":"
					if strings.HasPrefix(line, prefix) {
						rel, relErr := filepath.Rel(workspaceRoot, path)
						if relErr == nil {
							line = filepath.ToSlash(rel) + ":" + strings.TrimPrefix(line, prefix)
						}
						break
					}
				}
				out.WriteString(line)
				out.WriteByte('\n')
				matches++
				if matches == config.MaxMatches {
					truncated = true
					reachedLimit = true
					_ = command.Process.Kill()
					break
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					_ = command.Process.Kill()
					_ = command.Wait()
					return Result{}, readErr
				}
				break
			}
		}
		waitErr := command.Wait()
		if !reachedLimit && waitErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			var exitError *exec.ExitError
			if !errors.As(waitErr, &exitError) || exitError.ExitCode() != 1 {
				return Result{}, fmt.Errorf("rg: %w", waitErr)
			}
		}
		start = end
	}
	text := out.String()
	metadata := map[string]any{"matches": matches, "files": len(files)}
	if truncated {
		metadata["truncated"] = true
	}
	return Result{Text: text, ModelText: modelText(text), Metadata: metadata}, nil
}
