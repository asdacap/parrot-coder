package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type GrepConfig struct {
	MaxFiles     int
	MaxMatches   int
	MaxLineBytes int64
	MaxVisited   int
	Timeout      time.Duration
}
type GrepTool struct{ Config GrepConfig }

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
	return &GrepTool{c}
}
func (*GrepTool) ID() string { return "grep" }
func (*GrepTool) Description() string {
	return "Search workspace text files with Go RE2 regular expressions."
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
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`)
}

type grepInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}
type grepPlan struct {
	Input grepInput
	Root  string
	Regex *regexp.Regexp
}

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
	path := input.Path
	if path == "" {
		path = "."
	}
	root, err := call.Workspace.ResolveRead(path)
	if err != nil {
		return Plan{}, err
	}
	req, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "filesystem", Identifier: root, Operation: "search"}}, nil)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{req}, nil, grepPlan{input, root, rx})
}
func (t *GrepTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	p := plan.Data.(grepPlan)
	requestedPath := p.Input.Path
	if requestedPath == "" {
		requestedPath = "."
	}
	revalidated, err := call.Workspace.ResolveRead(requestedPath)
	if err != nil || revalidated != p.Root {
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
		files = []string{p.Root}
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
				resolved, e := call.Workspace.ResolveRead(path)
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
			line, e := readBoundedLine(reader, t.Config.MaxLineBytes)
			if e != nil && e != io.EOF {
				f.Close()
				return Result{}, e
			}
			if len(line) > 0 {
				lineNo++
				text := strings.TrimSuffix(line, "\n")
				if p.Regex.MatchString(text) {
					rel, _ := filepath.Rel(call.Workspace.Root(), path)
					out.WriteString(filepath.ToSlash(rel))
					out.WriteByte(':')
					out.WriteString(strconv.Itoa(lineNo))
					out.WriteByte(':')
					out.WriteString(text)
					out.WriteByte('\n')
					matches++
					if matches >= t.Config.MaxMatches {
						f.Close()
						return Result{Text: out.String(), Metadata: map[string]any{"matches": matches, "truncated": true}}, nil
					}
				}
			}
			if e == io.EOF {
				break
			}
		}
		f.Close()
	}
	return Result{Text: out.String(), Metadata: map[string]any{"matches": matches, "files": len(files)}}, nil
}
