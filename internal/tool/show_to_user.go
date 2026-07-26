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
	"strings"
)

// ShowToUserTool presents a bounded text-file range to the user as source code.
type ShowToUserTool struct {
	BasePresentation
	Config ReadConfig
}

func NewShowToUserTool(config ReadConfig) *ShowToUserTool {
	read := NewReadTool(config)
	return &ShowToUserTool{Config: read.Config}
}

func (*ShowToUserTool) ID() string { return "show_to_user" }
func (*ShowToUserTool) Presentation() Presentation {
	return Presentation{Muted: true, Label: LabelSpec{Fields: []LabelField{{Names: []string{"path"}}}}}
}
func (t *ShowToUserTool) Descriptor() Descriptor {
	return Descriptor{
		ID:           t.ID(),
		Description:  "Display a bounded line range from a text file to the user as source code without returning its content to the model. Relative paths resolve within the workspace.",
		Schema:       t.JSONSchema(),
		Presentation: t.Presentation(),
	}
}
func (*ShowToUserTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input readInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	description := fmt.Sprintf("Show %q to user", input.Path)
	if input.Offset > 0 || input.Limit > 0 {
		description += fmt.Sprintf(" (offset %d, limit %d)", input.Offset, input.Limit)
	}
	return description, nil
}
func (*ShowToUserTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"],"additionalProperties":false}`)
}
func (*ShowToUserTool) ErrorAdvice(raw json.RawMessage) (ErrorAdvice, error) {
	var input readInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ErrorAdvice{}, err
	}
	return ErrorAdvice{Paths: []ErrorAdvicePath{{Path: input.Path}}}, nil
}

func (t *ShowToUserTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("workspace is required")
	}
	var input readInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Offset < 0 || input.Limit < 0 {
		return Plan{}, errors.New("offset and limit must not be negative")
	}
	if input.Limit > t.Config.MaxLines {
		return Plan{}, errors.New("line limit exceeded")
	}
	path, err := call.Workspace.ResolveReadOnly(input.Path)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, readPlan{Input: input, Path: path})
}

func (t *ShowToUserTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	p := plan.Data.(readPlan)
	path, err := call.Workspace.ResolveReadOnly(p.Input.Path)
	if err != nil {
		return Result{}, err
	}
	if path != p.Path {
		return Result{}, errors.New("path changed after planning")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, err
	}
	if info.IsDir() {
		return Result{}, errors.New("cannot show a directory to the user")
	}
	file, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	probe := make([]byte, 8192)
	n, probeErr := file.Read(probe)
	if probeErr != nil && probeErr != io.EOF {
		return Result{}, probeErr
	}
	if isBinary(probe[:n]) {
		return Result{}, errors.New("binary file refused")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}

	offset, limit := p.Input.Offset, p.Input.Limit
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = t.Config.MaxLines
	}
	reader := bufio.NewReader(file)
	var source strings.Builder
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
			return Result{}, errors.New("show_to_user scan byte limit exceeded")
		}
		if len(line) > 0 {
			lineNo++
			if lineNo >= offset && returned < limit {
				if int64(source.Len()+len(line)) > t.Config.MaxBytes {
					return Result{}, errors.New("show_to_user byte limit exceeded")
				}
				source.WriteString(line)
				returned++
			}
		}
		if readErr == io.EOF || returned == limit {
			break
		}
	}

	language := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if call.Displays != nil {
		call.Displays.DisplayCode(CodeDisplay{Source: source.String(), Path: p.Input.Path, Language: language, StartLine: offset})
	}
	text := fmt.Sprintf("%d lines shown to user", returned)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"lines": returned, "path": p.Input.Path, "start_line": offset}}, nil
}
