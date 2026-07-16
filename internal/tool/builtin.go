package tool

import (
	"bufio"
	"bytes"
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

	"github.com/amirulashraf/parrot-coder/internal/permission"
)

type ReadConfig struct {
	MaxLines   int
	MaxBytes   int64
	MaxEntries int
}
type ReadTool struct{ Config ReadConfig }

func NewReadTool(config ReadConfig) *ReadTool {
	if config.MaxLines <= 0 {
		config.MaxLines = 2000
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = 1 << 20
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = 2000
	}
	return &ReadTool{config}
}
func (*ReadTool) ID() string { return "read" }
func (*ReadTool) Description() string {
	return "Read a bounded line range from a workspace file or list a directory."
}
func (*ReadTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input readInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	description := fmt.Sprintf("Read %q", input.Path)
	if input.Offset > 0 || input.Limit > 0 {
		description += fmt.Sprintf(" (offset %d, limit %d)", input.Offset, input.Limit)
	}
	return description, nil
}
func (*ReadTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"],"additionalProperties":false}`)
}

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}
type readPlan struct {
	Input readInput
	Path  string
}

func (t *ReadTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("workspace is required")
	}
	var input readInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	path, err := call.Workspace.ResolveRead(input.Path)
	if err != nil {
		return Plan{}, err
	}
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "filesystem", Identifier: path, Operation: "read"}}, nil)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, nil, readPlan{input, path})
}

func (t *ReadTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	p := plan.Data.(readPlan)
	revalidated, err := call.Workspace.ResolveRead(p.Input.Path)
	if err != nil || revalidated != p.Path {
		return Result{}, errors.New("path changed after planning")
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		return Result{}, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(p.Path)
		if err != nil {
			return Result{}, err
		}
		if len(entries) > t.Config.MaxEntries {
			return Result{}, errors.New("directory entry limit exceeded")
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		var b strings.Builder
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if int64(b.Len()+len(entry.Name())+2) > t.Config.MaxBytes {
				return Result{}, errors.New("directory listing byte limit exceeded")
			}
			b.WriteString(entry.Name())
			if entry.IsDir() {
				b.WriteByte('/')
			}
			b.WriteByte('\n')
		}
		return Result{Text: b.String(), Metadata: map[string]any{"entries": len(entries), "path": p.Input.Path}}, nil
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	probe := make([]byte, 8192)
	n, probeErr := f.Read(probe)
	if probeErr != nil && probeErr != io.EOF {
		return Result{}, probeErr
	}
	if isBinary(probe[:n]) {
		return Result{}, errors.New("binary file refused")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	offset := p.Input.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := p.Input.Limit
	if limit <= 0 {
		limit = t.Config.MaxLines
	}
	if limit > t.Config.MaxLines {
		return Result{}, errors.New("line limit exceeded")
	}
	reader := bufio.NewReader(f)
	var b strings.Builder
	lineNo, returned := 0, 0
	var scanned int64
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		line, readErr := readBoundedLine(reader, t.Config.MaxBytes)
		if readErr != nil && readErr != io.EOF {
			return Result{}, readErr
		}
		scanned += int64(len(line))
		if scanned > t.Config.MaxBytes {
			return Result{}, errors.New("read scan byte limit exceeded")
		}
		if len(line) > 0 {
			lineNo++
			if lineNo >= offset && returned < limit {
				text := fmt.Sprintf("%d: %s", lineNo, line)
				if int64(b.Len()+len(text)) > t.Config.MaxBytes {
					return Result{}, errors.New("read byte limit exceeded")
				}
				b.WriteString(text)
				returned++
			}
		}
		if readErr == io.EOF || returned == limit {
			break
		}
	}
	return Result{Text: b.String(), Metadata: map[string]any{"lines": returned, "path": p.Input.Path}}, nil
}

type GlobConfig struct {
	MaxResults int
	MaxVisited int
	Timeout    time.Duration
}
type GlobTool struct{ Config GlobConfig }

func NewGlobTool(config GlobConfig) *GlobTool {
	if config.MaxResults <= 0 {
		config.MaxResults = 1000
	}
	if config.MaxVisited <= 0 {
		config.MaxVisited = 100000
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	return &GlobTool{config}
}
func (*GlobTool) ID() string { return "glob" }
func (*GlobTool) Description() string {
	return "Find workspace paths with deterministic glob matching, including **."
}
func (*GlobTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input globInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Find workspace paths matching %q", input.Pattern), nil
}
func (*GlobTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`)
}

type globInput struct {
	Pattern string `json:"pattern"`
}
type globPlan struct {
	Input globInput
	Root  string
	Regex *regexp.Regexp
}

func (t *GlobTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("workspace is required")
	}
	var input globInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	rx, err := compileGlob(input.Pattern)
	if err != nil {
		return Plan{}, err
	}
	root, err := call.Workspace.ResolveRead(".")
	if err != nil {
		return Plan{}, err
	}
	req, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "filesystem", Identifier: root, Operation: "search"}}, nil)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{req}, nil, globPlan{input, root, rx})
}
func (t *GlobTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	p := plan.Data.(globPlan)
	revalidated, err := call.Workspace.ResolveRead(".")
	if err != nil || revalidated != p.Root {
		return Result{}, errors.New("glob root changed after planning")
	}
	ctx, cancel := context.WithTimeout(ctx, t.Config.Timeout)
	defer cancel()
	results := []string{}
	visited := 0
	err = filepath.WalkDir(p.Root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > t.Config.MaxVisited {
			return errors.New("glob traversal limit exceeded")
		}
		if path == p.Root {
			return nil
		}
		rel, err := filepath.Rel(p.Root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if p.Regex.MatchString(rel) {
			if _, err := call.Workspace.ResolveRead(path); err != nil {
				return nil
			}
			results = append(results, rel+map[bool]string{true: "/"}[d.IsDir()])
			if len(results) > t.Config.MaxResults {
				return errors.New("glob result limit exceeded")
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	sort.Strings(results)
	return Result{Text: strings.Join(results, "\n"), Metadata: map[string]any{"matches": len(results), "visited": visited}}, nil
}

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

type ReadOutputTool struct{ MaxBytes int64 }

func NewReadOutputTool(max int64) *ReadOutputTool {
	if max <= 0 {
		max = 1 << 20
	}
	return &ReadOutputTool{max}
}
func (*ReadOutputTool) ID() string { return "read_output" }
func (*ReadOutputTool) Description() string {
	return "Read a bounded byte range from an opaque managed output ID."
}
func (*ReadOutputTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input readOutputInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Read %d bytes at offset %d from managed output %q", input.Limit, input.Offset, input.ID), nil
}
func (*ReadOutputTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["id","offset","limit"],"additionalProperties":false}`)
}

type readOutputInput struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Limit  int64  `json:"limit"`
}

func (t *ReadOutputTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Outputs == nil {
		return Plan{}, errors.New("output store is required")
	}
	var input readOutputInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Limit > t.MaxBytes {
		return Plan{}, errors.New("output read limit exceeded")
	}
	req, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "managed_output", Identifier: input.ID, Operation: "read"}}, nil)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{req}, nil, input)
}
func (t *ReadOutputTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	input := plan.Data.(readOutputInput)
	b, err := call.Outputs.Read(input.ID, input.Offset, input.Limit)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: string(bytesToUTF8(b)), Metadata: map[string]any{"bytes": len(b)}}, nil
}

// DefaultWorkspacePolicy allows canonical read/search operations and reviewed
// workspace mutations made through edit or apply_patch. Other operations,
// including process execution, remain at ask.
func DefaultWorkspacePolicy() permission.Policy {
	return permission.Policy{Default: permission.Ask, Rules: []permission.Rule{{Match: func(r permission.Request) bool {
		if len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Operation != "read" && resource.Operation != "search" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "read-only operation"}, {Match: func(r permission.Request) bool {
		if r.ToolID != "edit" && r.ToolID != "apply_patch" || len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Kind != "filesystem" || resource.Operation != "write" && resource.Operation != "create" && resource.Operation != "delete" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "reviewed workspace mutation"}}}
}

// DefaultReadOnlyPolicy is retained for callers that need automatic access
// only to non-mutating workspace operations.
func DefaultReadOnlyPolicy() permission.Policy {
	policy := DefaultWorkspacePolicy()
	policy.Rules = policy.Rules[:1]
	return policy
}

func readBoundedLine(r *bufio.Reader, max int64) (string, error) {
	var b strings.Builder
	for {
		fragment, prefix, err := r.ReadLine()
		if int64(b.Len()+len(fragment)+1) > max {
			return "", errors.New("line byte limit exceeded")
		}
		b.Write(fragment)
		if !prefix {
			if err == nil {
				b.WriteByte('\n')
			}
			return b.String(), err
		}
		if err != nil {
			return b.String(), err
		}
	}
}
func isBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b)
}
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || strings.IndexByte(pattern, 0) >= 0 || filepath.IsAbs(pattern) {
		return nil, errors.New("invalid glob pattern")
	}
	clean := filepath.ToSlash(filepath.Clean(pattern))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, errors.New("glob traversal is not allowed")
	}
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(clean); {
		switch clean[i] {
		case '*':
			if i+1 < len(clean) && clean[i+1] == '*' {
				if i+2 < len(clean) && clean[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(clean[i : i+1]))
			i++
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}
