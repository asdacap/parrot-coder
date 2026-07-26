package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/process"
)

const writeStdinSchema = `{"type":"object","properties":{"process_id":{"type":"string","description":"Canonical process ID or friendly name of the running shell process."},"chars":{"type":"string","description":"Bytes to write to stdin. Defaults to empty, which polls without writing."},"yield_time_ms":{"type":"number","description":"Wait before yielding output. Non-empty writes default to 250 ms and cap at 30000 ms; empty polls wait 5000-300000 ms by default. The result will automatically be sent once done after the yield."},"max_output_tokens":{"type":"number","description":"Output token budget. Defaults to 10000 tokens; larger requests may be capped by policy."}},"required":["process_id"],"additionalProperties":false}`

type WriteStdinTool struct {
	BasePresentation
	Runner *process.Runner
}

type writeStdinInput struct {
	ProcessID       string `json:"process_id"`
	Chars           string `json:"chars"`
	YieldTimeMS     uint64 `json:"yield_time_ms"`
	MaxOutputTokens *int   `json:"max_output_tokens"`
}

func NewWriteStdinTool(runner *process.Runner) *WriteStdinTool {
	return &WriteStdinTool{Runner: runner}
}

func (*WriteStdinTool) ID() string { return "write_stdin" }
func (*WriteStdinTool) Presentation() Presentation {
	return Presentation{
		Redact:            []string{"chars", "input"},
		Output:            OutputNone,
		LabelInPermission: true,
		Label: LabelSpec{Fields: []LabelField{
			{Names: []string{"process_id"}, TaskName: true},
			{Names: []string{"chars"}},
		}},
	}
}

func (t *WriteStdinTool) Descriptor() Descriptor {
	return Descriptor{
		ID:           t.ID(),
		Description:  "Writes characters to a running shell process and returns recent output.",
		Schema:       t.JSONSchema(),
		Presentation: t.Presentation(),
	}
}

func (*WriteStdinTool) JSONSchema() json.RawMessage { return json.RawMessage(writeStdinSchema) }

func (*WriteStdinTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input writeStdinInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Write %d characters to process %s", len([]rune(input.Chars)), input.ProcessID), nil
}

func (t *WriteStdinTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input writeStdinInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.ProcessID == "" {
		return Plan{}, errors.New("write_stdin: process_id is required")
	}
	if input.YieldTimeMS == 0 {
		input.YieldTimeMS = uint64(process.DefaultWriteYieldTime / time.Millisecond)
	}
	if input.MaxOutputTokens != nil && *input.MaxOutputTokens < 0 {
		return Plan{}, errors.New("write_stdin: max_output_tokens must be nonnegative")
	}
	review, _ := json.Marshal(map[string]any{
		"process_id": input.ProcessID, "character_count": len([]rune(input.Chars)),
		"yield_time_ms": input.YieldTimeMS, "max_output_tokens": input.MaxOutputTokens,
	})
	return NewPlan(t.ID(), raw, nil, review, input)
}

func (t *WriteStdinTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	runner := t.Runner
	if runner == nil {
		runner = call.Processes
	}
	if runner == nil {
		return Result{}, errors.New("write_stdin: process runner is required")
	}
	input := plan.Data.(writeStdinInput)
	result, err := runner.WritePersistent(ctx, process.PersistentWriteRequest{
		SessionID: call.SessionID, ProcessID: input.ProcessID, Chars: input.Chars,
		Yield:           time.Duration(input.YieldTimeMS) * time.Millisecond,
		MaxOutputTokens: input.MaxOutputTokens, Output: call.Output,
	})
	if err != nil {
		return Result{}, fmt.Errorf("write_stdin failed: %w", err)
	}
	text := formatPersistentResult(result)
	return Result{Text: text, ModelText: modelText(text)}, nil
}
