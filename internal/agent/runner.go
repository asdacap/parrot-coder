package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/queue"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type QueueManager interface {
	tool.QueueService
	Directory() string
	DeliverMonitored(func(queue.Notification) (bool, error)) (bool, error)
}

type StatusObserver interface {
	Observe(context.Context, statusinfo.Query, statusinfo.Provider) (string, error)
}

type SystemContextPrompt interface {
	GetSystemContextPrompt(context.Context) (string, error)
}

type LivePublisher interface {
	PublishProtocol(string, protocol.Event)
}

type noopLivePublisher struct{}

func (noopLivePublisher) PublishProtocol(string, protocol.Event) {}

type toolOutputWriter struct {
	live      LivePublisher
	sessionID string
	callID    string
	mu        sync.Mutex
	pending   []byte
}

func (w *toolOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	complete := len(w.pending)
	if complete > 0 {
		start := complete - 1
		for start > 0 && !utf8.RuneStart(w.pending[start]) {
			start--
		}
		if !utf8.FullRune(w.pending[start:]) {
			complete = start
		}
	}
	if complete > 0 && w.live != nil {
		w.live.PublishProtocol(w.sessionID, protocol.Event{Type: protocol.EventToolOutputDelta, ToolCallID: w.callID, Text: string(w.pending[:complete])})
	}
	w.pending = append(w.pending[:0], w.pending[complete:]...)
	return len(p), nil
}

type toolDisplayPublisher struct {
	live      LivePublisher
	sessionID string
	callID    string
}

func (p toolDisplayPublisher) DisplayCode(display tool.CodeDisplay) {
	if p.live == nil {
		return
	}
	p.live.PublishProtocol(p.sessionID, protocol.Event{Type: protocol.EventCodeDisplay, CodeDisplay: &protocol.CodeDisplay{
		ToolCallID: p.callID, Source: display.Source, Path: display.Path, Language: display.Language, StartLine: display.StartLine,
	}})
}

type Compactor interface {
	Compact(context.Context, compaction.Request) (compaction.Result, error)
	RepairActive(context.Context, string) error
}

type ProfileResolver interface {
	GetProfile(string) (Profile, error)
}

type AgentSessionConfig struct {
	ToolMaxInputBytes  int
	ToolMaxOutputBytes int
	MaxConcurrentTools int
	CleanupTimeout     time.Duration
}

func applyAgentSessionDefaults(config *AgentSessionConfig) {
	if config.MaxConcurrentTools <= 0 {
		config.MaxConcurrentTools = 4
	}
	if config.CleanupTimeout <= 0 {
		config.CleanupTimeout = 5 * time.Second
	}
}

type completedCall struct {
	messageID string
	call      protocol.ToolCall
}

type providerTurnFailure struct {
	err       error
	code      string
	overflow  bool
	retrySafe bool
}

func (e *providerTurnFailure) Error() string { return e.err.Error() }
func (e *providerTurnFailure) Unwrap() error { return e.err }

func contextOverflowError(err error) (string, bool) {
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 400 && httpErr.StatusCode != 413 {
		return "", false
	}
	return httpErr.Code, canonicalOverflow(httpErr.Type, httpErr.Code) || overflowMessage(httpErr.Message)
}

func canonicalOverflow(kind, code string) bool {
	for _, value := range []string{strings.ToLower(kind), strings.ToLower(code)} {
		switch value {
		case "context_length_exceeded", "context_window_exceeded", "prompt_too_long", "input_too_long", "max_context_length_exceeded":
			return true
		}
	}
	return false
}

// overflowMessage recognizes providers that report a context overflow only in
// the human-readable error message rather than a canonical type or code (for
// example Kimi: "Your request exceeded model token limit"). Phrases are kept
// specific to input size so unrelated invalid-request errors do not trigger a
// wasted compaction and retry.
func overflowMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, phrase := range []string{
		"exceeded model token limit", "token limit", "context length", "context window",
		"maximum context", "prompt is too long", "too many tokens", "reduce the length of the messages",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func runnerInstructions(systemPrompt, summaryPrompt, scratchPath, queuesPath string, final bool) string {
	sections := make([]string, 0, 5)
	for _, section := range []string{systemPrompt, summaryPrompt, "Scratch directory: " + scratchPath, "Queues directory (read-only; use queue tools to modify): " + queuesPath, finalTurnInstructions(final)} {
		if section != "" {
			sections = append(sections, section)
		}
	}
	return strings.Join(sections, "\n\n")
}

func finalTurnInstructions(final bool) string {
	if final {
		return "This is the final turn. Do not call tools; provide the best final answer now."
	}
	return ""
}

type profileStatus struct {
	prompt    string
	hardRules []string
	provider  statusinfo.Provider
}

func newProfileStatus(profile Profile) statusinfo.Provider {
	return profileStatus{prompt: profile.Prompt(), hardRules: profile.HardRules(), provider: profile.Status()}
}

func (s profileStatus) Key() string {
	if s.provider != nil {
		return s.provider.Key()
	}
	return "profile:instructions"
}

func (s profileStatus) Observe(ctx context.Context, query statusinfo.Query) (statusinfo.Observation, error) {
	sections := make([]string, 0, 2)
	instructions := s.prompt
	if len(s.hardRules) > 0 {
		instructions += "\n\nHard rules:\n- " + strings.Join(s.hardRules, "\n- ")
	}
	if instructions != "" {
		sections = append(sections, instructions)
	}
	if s.provider != nil {
		observation, err := s.provider.Observe(ctx, query)
		if err != nil {
			return statusinfo.Observation{}, err
		}
		if observation.Available && strings.TrimSpace(observation.Text) != "" {
			sections = append(sections, observation.Text)
		}
	}
	return statusinfo.Observation{Available: len(sections) > 0, Text: strings.Join(sections, "\n\n")}, nil
}

func finalParts(text, reasoning string, calls []completedCall) []protocol.ContentPart {
	var parts []protocol.ContentPart
	if reasoning != "" {
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentReasoning, Text: reasoning})
	}
	if text != "" {
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentText, Text: text})
	}
	for _, call := range calls {
		copy := call.call
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentToolCall, ToolCall: &copy})
	}
	return parts
}

func preferredReasoning(reasoning, summary string) string {
	if summary != "" {
		return summary
	}
	return reasoning
}

type reasoningSummaryAccumulator struct {
	parts map[string]*strings.Builder
	order []string
	bytes int
}

func (a *reasoningSummaryAccumulator) Write(partID, text string) {
	if text == "" {
		return
	}
	if a.parts == nil {
		a.parts = make(map[string]*strings.Builder)
	}
	part := a.parts[partID]
	if part == nil {
		part = &strings.Builder{}
		a.parts[partID] = part
		a.order = append(a.order, partID)
	}
	part.WriteString(text)
	a.bytes += len(text)
}

func (a *reasoningSummaryAccumulator) Set(partID, text string) {
	if a.parts == nil {
		a.parts = make(map[string]*strings.Builder)
	}
	part := a.parts[partID]
	if part == nil {
		part = &strings.Builder{}
		a.parts[partID] = part
		a.order = append(a.order, partID)
	} else {
		a.bytes -= part.Len()
		part.Reset()
	}
	part.WriteString(text)
	a.bytes += len(text)
}

func (a *reasoningSummaryAccumulator) String() string {
	var summary strings.Builder
	summary.Grow(a.bytes)
	for _, partID := range a.order {
		summary.WriteString(a.parts[partID].String())
	}
	return summary.String()
}

func (a *reasoningSummaryAccumulator) Len() int { return a.bytes }

type toolOutcome struct {
	call        completedCall
	text        string
	modelText   string
	err         error
	interrupted bool
	settled     bool
	persistErr  error
}

// executeToolCall runs one tool and converts any panic in its Plan or Execute
// into a normal error. A tool call executes in its own goroutine, so without
// this recovery a single panicking tool would terminate the entire process
// rather than failing just that call. The recovered stack is captured in the
// error so the underlying defect remains diagnosable.
func executeToolCall(ctx context.Context, executor tool.Executor, call completedCall, callContext tool.CallContext, onPanic func(recovered any, stack []byte)) (result tool.Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			if onPanic != nil {
				onPanic(recovered, stack)
			}
			result = tool.Result{}
			err = fmt.Errorf("tool %q panicked: %v\n%s", call.call.Name, recovered, stack)
		}
	}()
	return executor.Execute(ctx, call.call.Name, json.RawMessage(call.call.Input), callContext)
}

func toolResultMessage(outcome toolOutcome) protocol.Message {
	text := outcome.modelText
	if outcome.interrupted {
		reason := "tool execution interrupted"
		if outcome.err != nil {
			reason += ": " + outcome.err.Error()
		}
		text = "Error: " + reason
	} else if outcome.err != nil {
		text = "Error: " + outcome.err.Error()
	}
	return protocol.Message{Role: protocol.RoleTool, Content: []protocol.ContentPart{{Type: protocol.ContentToolResult, Text: text, ToolCallID: outcome.call.call.ID}}}
}

func toolDefinitions(snapshot tool.Snapshot) []protocol.ToolDefinition {
	definitions := snapshot.Definitions()
	result := make([]protocol.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, protocol.ToolDefinition{Name: definition.ID, Description: definition.Description, InputSchema: definition.Schema})
	}
	return result
}
