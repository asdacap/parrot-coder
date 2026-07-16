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
	"sort"
	"strings"
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
