// Package tool plans, authorizes, and executes bounded tools.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type CallContext struct {
	Workspace  *workspace.Workspace
	Outputs    *OutputStore
	SessionID  string
	Changes    *change.Service
	Processes  *process.Runner
	Todos      *session.TodoService
	Questions  *question.Broker
	Agent      string
	ToolCallID string
	Output     io.Writer
	SecurityProfile security.SecurityProfile
}

type Plan struct {
	ToolID         string
	CanonicalInput json.RawMessage
	OperationHash  string
	Permissions    []permission.Request
	Review         json.RawMessage
	Data           any
}

// Result carries a tool's output to two audiences. Text is the complete record
// kept by the session and shown to the user. ModelText is what the model reads.
// A tool returning any output must set both: there is no fallback, because only
// the tool knows which part of its output the model must retain and how large
// that copy may safely grow. A tool producing no output at all may leave both
// empty; the executor substitutes a placeholder so the model never reads an
// empty result.
type Result struct {
	Text      string         `json:"text"`
	ModelText string         `json:"model_text"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type Tool interface {
	ID() string
	Description() string
	// DescribeRequest decodes this tool's parameters and returns concise,
	// human-readable permission context for the invocation. Implementations must
	// not generically serialize the JSON input or expose credentials and secret
	// values merely because they occur in it.
	DescribeRequest(json.RawMessage) (string, error)
	JSONSchema() json.RawMessage
	// Presentation declares display-only metadata so that renderers branch on
	// what a tool does rather than on which tool it is. Embed BasePresentation
	// for the neutral default.
	Presentation() Presentation
	Plan(context.Context, json.RawMessage, CallContext) (Plan, error)
	Execute(context.Context, Plan, CallContext) (Result, error)
}

func NewPlan(toolID string, input json.RawMessage, requests []permission.Request, review json.RawMessage, data any) (Plan, error) {
	canonical, err := permission.CanonicalJSON(input)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{ToolID: toolID, CanonicalInput: canonical, Permissions: requests, Review: review, Data: data}
	p.OperationHash, err = planHash(p)
	return p, err
}

func planHash(p Plan) (string, error) {
	resources := make([]permission.Resource, 0, len(p.Permissions))
	for _, request := range p.Permissions {
		resources = append(resources, permission.Resource{Kind: "permission", Identifier: request.OperationHash, Operation: "authorize"})
	}
	r, err := permission.NewRequest(p.ToolID, p.CanonicalInput, resources, p.Review)
	if err != nil {
		return "", err
	}
	return r.OperationHash, nil
}

type Definition struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type Registry struct {
	mu           sync.Mutex
	tools        map[string]Tool
	materialized bool
}

func NewRegistry() *Registry { return &Registry{tools: make(map[string]Tool)} }

func (r *Registry) Register(t Tool) error {
	if t == nil || t.ID() == "" {
		return errors.New("tool and tool ID are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.materialized {
		return errors.New("registry already materialized")
	}
	if _, exists := r.tools[t.ID()]; exists {
		return fmt.Errorf("duplicate tool ID %q", t.ID())
	}
	if _, err := parseSchema(t.JSONSchema()); err != nil {
		return fmt.Errorf("tool %s schema: %w", t.ID(), err)
	}
	r.tools[t.ID()] = t
	return nil
}

func (r *Registry) Definitions() []Definition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return definitions(r.tools)
}

type Snapshot struct {
	tools       map[string]Tool
	definitions []Definition
}

func (r *Registry) Materialize() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.materialized = true
	tools := make(map[string]Tool, len(r.tools))
	for id, t := range r.tools {
		tools[id] = t
	}
	return Snapshot{tools: tools, definitions: definitions(tools)}
}

func (s Snapshot) Definitions() []Definition {
	out := make([]Definition, len(s.definitions))
	for i, definition := range s.definitions {
		out[i] = definition
		out[i].Schema = append(json.RawMessage(nil), definition.Schema...)
	}
	return out
}

// Presentations projects the declared display metadata of every tool. It is
// deliberately separate from Definitions so that presentation detail never
// reaches the model's tool guidance.
func (s Snapshot) Presentations() []PresentationEntry {
	out := make([]PresentationEntry, 0, len(s.tools))
	for id, t := range s.tools {
		out = append(out, PresentationEntry{ID: id, Presentation: t.Presentation()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func definitions(tools map[string]Tool) []Definition {
	out := make([]Definition, 0, len(tools))
	for _, t := range tools {
		out = append(out, Definition{ID: t.ID(), Description: t.Description(), Schema: append(json.RawMessage(nil), t.JSONSchema()...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type Authorizer interface {
	Authorize(context.Context, permission.Request) (permission.Decision, error)
}

type Executor struct {
	Snapshot       Snapshot
	Permissions    Authorizer
	MaxInputBytes  int
	MaxOutputBytes int
}

func (e Executor) Execute(ctx context.Context, id string, raw json.RawMessage, call CallContext) (result Result, err error) {
	started := time.Now()
	diagnostics.Event("tool_execution_started",
		"session_id", call.SessionID, "tool_call_id", call.ToolCallID, "tool", id, "input_bytes", len(raw),
	)
	defer func() {
		if recovered := recover(); recovered != nil {
			diagnostics.Panic("tool_executor", recovered)
			panic(recovered)
		}
		status := "success"
		if ctx.Err() != nil {
			status = "interrupted"
		} else if err != nil {
			status = "error"
		}
		attributes := []any{
			"session_id", call.SessionID, "tool_call_id", call.ToolCallID, "tool", id,
			"status", status, "duration_ms", time.Since(started).Milliseconds(), "output_bytes", len(result.Text),
		}
		if err != nil {
			diagnostics.Error("tool_execution_finished", append(attributes, "error_type", diagnostics.ErrorType(err))...)
		} else {
			diagnostics.Event("tool_execution_finished", attributes...)
		}
	}()
	t, ok := e.Snapshot.tools[id]
	if !ok {
		return Result{}, fmt.Errorf("unknown tool %q", id)
	}
	maxInput := e.MaxInputBytes
	if maxInput <= 0 {
		maxInput = 1 << 20
	}
	if len(raw) > maxInput {
		return Result{}, errors.New("tool input byte limit exceeded")
	}
	if err := checkJSONDepth(raw, 64); err != nil {
		return Result{}, err
	}
	if err := validateJSON(t.JSONSchema(), raw); err != nil {
		return Result{}, fmt.Errorf("invalid tool input: %w", err)
	}
	p, err := t.Plan(ctx, raw, call)
	if err != nil {
		return Result{}, err
	}
	if p.ToolID != id {
		return Result{}, errors.New("plan tool ID mismatch")
	}
	hash, err := planHash(p)
	if err != nil || hash != p.OperationHash {
		return Result{}, errors.New("stale plan operation hash")
	}
	if len(p.Permissions) > 0 {
		description, err := t.DescribeRequest(p.CanonicalInput)
		if err != nil {
			return Result{}, fmt.Errorf("describe tool request: %w", err)
		}
		description = strings.TrimSpace(description)
		if description == "" {
			return Result{}, errors.New("tool request description is required for permission")
		}
		for i := range p.Permissions {
			if err := p.Permissions[i].Verify(); err != nil {
				return Result{}, err
			}
			p.Permissions[i].Description = description
			p.Permissions[i].Choices = ChoicesFor(t)
			p.Permissions[i].OperationHash, err = permission.Hash(p.Permissions[i])
			if err != nil {
				return Result{}, err
			}
		}
		p.OperationHash, err = planHash(p)
		if err != nil {
			return Result{}, err
		}
	}
	for _, request := range p.Permissions {
		if err := request.Verify(); err != nil {
			return Result{}, err
		}
		request.SessionID = call.SessionID
		if call.Workspace != nil {
			request.Workspace = call.Workspace.Root()
		}
		if e.Permissions == nil {
			return Result{}, errors.New("permission broker is required")
		}
		decision, err := e.Permissions.Authorize(ctx, request)
		if err != nil {
			return Result{}, err
		}
		if decision != permission.Allow {
			return Result{}, errors.New("tool permission denied")
		}
	}
	result, err = t.Execute(ctx, p, call)
	if err != nil {
		return Result{}, err
	}
	max := e.MaxOutputBytes
	if max <= 0 {
		max = 1 << 20
	}
	if result.ModelText == "" && result.Text != "" {
		return Result{}, fmt.Errorf("tool %q returned output without a model copy", id)
	}
	if result.ModelText == "" {
		result.ModelText = emptyModelText
	}
	// Text is never truncated here, and bounding the model copy is each tool's
	// responsibility. Spilling is the exception: the executor performed the spill
	// and is the only party holding the resulting identifier, so it replaces the
	// model copy to report what it did. Without this the model receives a
	// preview it cannot act on, because nothing else names the output ID.
	if len(result.Text) > max && call.Outputs != nil {
		if result.Metadata == nil {
			result.Metadata = make(map[string]any)
		}
		stored, storeErr := call.Outputs.Store(ctx, strings.NewReader(result.Text))
		if storeErr != nil {
			result.Metadata["output_lossy"] = true
			result.ModelText = modelText(result.ModelText) +
				fmt.Sprintf("\n... output exceeded %d bytes and could not be stored; the remainder is unrecoverable ...", max)
		} else {
			result.Metadata["output_id"] = stored.ID
			result.Metadata["output_bytes"] = stored.Size
			lost := ""
			if stored.OmittedBytes > 0 {
				result.Metadata["output_lossy"] = true
				lost = fmt.Sprintf("\n... first %d bytes could not be stored ...", stored.OmittedBytes)
			}
			result.ModelText = modelText(stored.Preview) + lost +
				fmt.Sprintf("\n... %d bytes total; read the remainder with read_output id %s ...", stored.Size, stored.ID)
		}
	}
	if len(result.ModelText) > max {
		diagnostics.Warn("tool_model_text_unbounded",
			"session_id", call.SessionID, "tool_call_id", call.ToolCallID, "tool", id,
			"model_text_bytes", len(result.ModelText), "limit_bytes", max,
		)
	}
	return result, nil
}

func boundedText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	suffix := "\n... truncated ..."
	if len(suffix) >= max {
		suffix = ""
	}
	cut := max - len(suffix)
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + suffix
}

func checkJSONDepth(raw []byte, max int) error {
	depth := 0
	inString, escaped := false, false
	for _, c := range raw {
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > max {
				return errors.New("tool input JSON depth limit exceeded")
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

type schema struct {
	Type                 string            `json:"type"`
	Properties           map[string]schema `json:"properties"`
	Required             []string          `json:"required"`
	AdditionalProperties json.RawMessage   `json:"additionalProperties"`
	Items                *schema           `json:"items"`
}

func parseSchema(raw []byte) (schema, error) {
	var s schema
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(&s); err != nil {
		return s, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return s, errors.New("trailing schema data")
	}
	if _, err := permission.CanonicalJSON(raw); err != nil {
		return s, err
	}
	if s.Type == "" {
		return s, errors.New("schema type is required")
	}
	return s, nil
}

func validateJSON(rawSchema, raw []byte) error {
	s, err := parseSchema(rawSchema)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return validateValue(s, value, "input")
}

func validateValue(s schema, value any, path string) error {
	switch s.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		for _, key := range s.Required {
			if _, ok := object[key]; !ok {
				return fmt.Errorf("%s.%s is required", path, key)
			}
		}
		for key, child := range object {
			property, known := s.Properties[key]
			if !known {
				additional, allowed, err := additionalSchema(s.AdditionalProperties)
				if err != nil {
					return err
				}
				if !allowed {
					return fmt.Errorf("unknown field %q", key)
				}
				if additional != nil {
					if err := validateValue(*additional, child, path+"."+key); err != nil {
						return err
					}
				}
				continue
			}
			if err := validateValue(property, child, path+"."+key); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be string", path)
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be integer", path)
		}
		if _, err := n.Int64(); err != nil {
			return fmt.Errorf("%s must be integer", path)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s must be number", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", path)
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", path)
		}
		if s.Items != nil {
			for i, item := range items {
				if err := validateValue(*s.Items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("unsupported schema type %q", s.Type)
	}
	return nil
}

func additionalSchema(raw json.RawMessage) (*schema, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var allowed bool
	if err := json.Unmarshal(raw, &allowed); err == nil {
		return nil, allowed, nil
	}
	var value schema
	if err := json.Unmarshal(raw, &value); err != nil || value.Type == "" {
		return nil, false, errors.New("invalid additionalProperties schema")
	}
	return &value, true, nil
}
