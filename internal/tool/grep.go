package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type GrepConfig struct {
	MaxFiles     int
	MaxMatches   int
	MaxLineBytes int64
	MaxVisited   int
	Timeout      time.Duration
}
type GrepTool struct {
	BasePresentation
	Config GrepConfig
}

func NewGrepTool(c GrepConfig) *GrepTool {
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
	return &GrepTool{Config: c}
}
func (*GrepTool) ID() string { return "grep" }
func (*GrepTool) Presentation() Presentation {
	return Presentation{
		Muted: true,
		Label: LabelSpec{Fields: []LabelField{
			{Names: []string{"pattern"}, Quote: true},
			{Names: []string{"path"}, Default: "."},
		}},
	}
}

func (*GrepTool) Description() string {
	return "Search text files with Go RE2 regular expressions. Relative paths resolve within the workspace; include optionally filters files with a glob."
}
func (*GrepTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input grepInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	path := input.Path
	if path == "" {
		path = "."
	}
	return fmt.Sprintf("Search %q for pattern %q", path, input.Pattern), nil
}
func (*GrepTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"include":{"type":"string","description":"Optional glob selecting files to search."}},"required":["pattern"],"additionalProperties":false}`)
}
func (*GrepTool) ErrorAdvice(raw json.RawMessage) (ErrorAdvice, error) {
	var input grepInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ErrorAdvice{}, err
	}
	if input.Path == "" {
		input.Path = "."
	}
	return ErrorAdvice{Paths: []ErrorAdvicePath{{Path: input.Path}}}, nil
}

type grepInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Include string `json:"include"`
}
type grepPlan struct {
	Input       grepInput
	Root        string
	Regex       *regexp.Regexp
	Include     *regexp.Regexp
	IncludeBase bool
}

func (p grepPlan) includes(path string) bool {
	if p.Include == nil {
		return true
	}
	candidate := filepath.ToSlash(path)
	if p.IncludeBase {
		candidate = filepath.Base(path)
	}
	return p.Include.MatchString(candidate)
}

type grepLineReader struct {
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

func newGrepLineReader(ctx context.Context, reader *bufio.Reader, maxPrefix int64) *grepLineReader {
	return &grepLineReader{ctx: ctx, reader: reader, maxPrefix: maxPrefix}
}

func (r *grepLineReader) ReadRune() (rune, int, error) {
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

func (r *grepLineReader) drain() error {
	for !r.done {
		if _, _, err := r.ReadRune(); err != nil && err != io.EOF {
			break
		}
	}
	return r.err
}

func (r *grepLineReader) truncated() bool { return r.prefixStopped || r.length > r.maxPrefix }

func (t *GrepTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("workspace is required")
	}
	var input grepInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	rx, err := regexp.Compile(input.Pattern)
	if err != nil {
		return Plan{}, err
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
	return NewPlan(t.ID(), raw, nil, nil, grepPlan{
		Input:       input,
		Root:        root,
		Regex:       rx,
		Include:     include,
		IncludeBase: !strings.Contains(filepath.ToSlash(input.Include), "/"),
	})
}
func (t *GrepTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {

	p := plan.Data.(grepPlan)
	requestedPath := p.Input.Path
	if requestedPath == "" {
		requestedPath = "."
	}
	revalidated, err := call.Workspace.ResolveReadOnly(requestedPath)
	if err != nil {
		return Result{}, err
	}
	if revalidated != p.Root {
		return Result{}, errors.New("grep root changed after planning")
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
				return errors.New("grep traversal limit exceeded")
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
						return errors.New("grep file limit exceeded")
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
			line := newGrepLineReader(ctx, reader, t.Config.MaxLineBytes)
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
