// Package tool plans, authorizes, and executes bounded tools.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

// CodeDisplay describes source a tool wants a client to present. The publisher
// supplies execution identity such as the session and tool call IDs.
type CodeDisplay struct {
	Source    string
	Path      string
	Language  string
	StartLine int
}

// DisplayPublisher is the presentation boundary available to tools. Publishing
// is best-effort and does not change the tool's authoritative result.
type DisplayPublisher interface {
	DisplayCode(CodeDisplay)
}

type CallContext struct {
	Workspace       *workspace.Workspace
	Outputs         *OutputStore
	SessionID       string
	TaskID          string
	Changes         *change.Service
	Processes       *process.Runner
	Todos           *session.TodoService
	Questions       *question.Broker
	Agent           string
	ToolCallID      string
	Output          io.Writer
	Displays        DisplayPublisher
	SecurityProfile security.SecurityProfile
	StatusQuery     statusinfo.Query
	StatusProvider  statusinfo.Provider
}

type Plan struct {
	ToolID         string
	CanonicalInput json.RawMessage
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

// Descriptor is the immutable, session-independent description of one tool.
// Providers expose descriptors so callers can advertise tools without creating
// execution-capable instances.
type Descriptor struct {
	ID                   string
	Description          string
	Schema               json.RawMessage
	Presentation         Presentation
	SystemPromptGuidance string
}

func (d Descriptor) clone() Descriptor {
	d.Schema = append(json.RawMessage(nil), d.Schema...)
	d.Presentation = d.Presentation.clone()
	return d
}

// AgentRelationship describes how an agent message target is related to the
// session whose tools are resolving it.
type AgentRelationship string

const (
	AgentRelationshipParent     AgentRelationship = "parent"
	AgentRelationshipDescendant AgentRelationship = "descendant"
)

// ResolvedAgent retains both the canonical target and the relationship used to
// authorize access to it.
type ResolvedAgent struct {
	Agent        ChildAgent
	Relationship AgentRelationship
}

// AgentSession is the agent-session capability passed to tool providers. It is
// deliberately owned by this package so tool does not depend on agent.
type AgentSession interface {
	SessionID() string
	CreateAgent(context.Context, string, string, string, string, string) (ChildAgent, error)
	ResolveAgent(string) (ResolvedAgent, error)
}

type ToolProvider interface {
	Descriptor() Descriptor
	// Create returns a fresh executable instance bound to the supplied session.
	Create(AgentSession) (Tool, error)
}

type ProviderFunc struct {
	ToolDescriptor Descriptor
	CreateTool     func(AgentSession) (Tool, error)
}

func (p *ProviderFunc) Descriptor() Descriptor { return p.ToolDescriptor.clone() }
func (p *ProviderFunc) Create(state AgentSession) (Tool, error) {
	if p == nil || p.CreateTool == nil {
		return nil, errors.New("tool: provider create function is required")
	}
	return p.CreateTool(state)
}

type catalogProvider struct {
	descriptor Descriptor
	create     func(AgentSession) (Tool, error)
}

func (p catalogProvider) Descriptor() Descriptor                  { return p.descriptor.clone() }
func (p catalogProvider) Create(state AgentSession) (Tool, error) { return p.create(state) }

// Providers is a validated, immutable provider catalog.
type Providers struct {
	items     []catalogProvider
	validated bool
}

func NewProviders(items ...ToolProvider) (Providers, error) {
	seen := make(map[string]struct{}, len(items))
	copied := make([]catalogProvider, 0, len(items))
	for i, provider := range items {
		if provider == nil {
			return Providers{}, fmt.Errorf("tool: provider %d is nil", i)
		}
		descriptor := provider.Descriptor()
		if descriptor.ID == "" {
			return Providers{}, fmt.Errorf("tool: provider %d ID is required", i)
		}
		if _, exists := seen[descriptor.ID]; exists {
			return Providers{}, fmt.Errorf("duplicate tool ID %q", descriptor.ID)
		}
		if _, err := parseSchema(descriptor.Schema); err != nil {
			return Providers{}, fmt.Errorf("tool %s schema: %w", descriptor.ID, err)
		}
		seen[descriptor.ID] = struct{}{}
		descriptor = descriptor.clone()
		create := provider.Create
		if providerFunc, ok := provider.(*ProviderFunc); ok {
			create = providerFunc.CreateTool
		}
		if create == nil {
			return Providers{}, fmt.Errorf("tool: provider %s create function is required", descriptor.ID)
		}
		copied = append(copied, catalogProvider{descriptor: descriptor, create: create})
	}
	return Providers{items: copied, validated: true}, nil
}

func (p Providers) Valid() bool { return p.validated }

func (p Providers) Without(blacklisted []string) Providers {
	if len(blacklisted) == 0 {
		return p
	}
	excluded := make(map[string]bool, len(blacklisted))
	for _, id := range blacklisted {
		excluded[id] = true
	}
	items := make([]catalogProvider, 0, len(p.items))
	for _, provider := range p.items {
		if !excluded[provider.Descriptor().ID] {
			items = append(items, provider)
		}
	}
	return Providers{items: items, validated: p.validated}
}

func (p Providers) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(p.items))
	for _, provider := range p.items {
		out = append(out, provider.Descriptor())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p Providers) Definitions() []Definition { return descriptorDefinitions(p.Descriptors()) }
func (p Providers) Presentations() []PresentationEntry {
	out := make([]PresentationEntry, 0, len(p.items))
	for _, descriptor := range p.Descriptors() {
		out = append(out, PresentationEntry{ID: descriptor.ID, Presentation: descriptor.Presentation})
	}
	return out
}
func (p Providers) SystemPromptGuidance() string {
	var entries []string
	for _, descriptor := range p.Descriptors() {
		if descriptor.SystemPromptGuidance != "" {
			entries = append(entries, descriptor.SystemPromptGuidance)
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n\n")
}

func (p Providers) Materialize(state AgentSession) (Snapshot, error) {
	tools := make(map[string]Tool, len(p.items))
	for _, provider := range p.items {
		descriptor := provider.Descriptor()
		created, err := provider.Create(state)
		if err != nil {
			return Snapshot{}, fmt.Errorf("tool: create %s: %w", descriptor.ID, err)
		}
		if created == nil {
			return Snapshot{}, fmt.Errorf("tool: provider %s returned nil", descriptor.ID)
		}
		if created.ID() != descriptor.ID || created.Description() != descriptor.Description || !bytes.Equal(created.JSONSchema(), descriptor.Schema) || !reflect.DeepEqual(created.Presentation(), descriptor.Presentation) || created.SystemPromptGuidance() != descriptor.SystemPromptGuidance {
			return Snapshot{}, fmt.Errorf("tool: provider %s returned inconsistent tool", descriptor.ID)
		}
		tools[descriptor.ID] = created
	}
	return Snapshot{tools: tools, definitions: definitions(tools)}, nil
}

func DescriptorOf(t Tool) Descriptor {
	if descriptor := t.Descriptor(); descriptor.ID != "" {
		return descriptor.clone()
	}
	return Descriptor{ID: t.ID(), Description: t.Description(), Schema: append(json.RawMessage(nil), t.JSONSchema()...), Presentation: t.Presentation().clone(), SystemPromptGuidance: t.SystemPromptGuidance()}
}

func descriptorDefinitions(descriptors []Descriptor) []Definition {
	out := make([]Definition, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, Definition{ID: descriptor.ID, Description: descriptor.Description, Schema: append(json.RawMessage(nil), descriptor.Schema...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type Tool interface {
	Descriptor() Descriptor
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
	// SystemPromptGuidance returns extra text injected into the system prompt
	// to explain this tool's runtime behavior beyond its schema. Return "" to
	// opt out; only non-empty guidance is included. Embed BasePresentation for
	// the neutral default.
	SystemPromptGuidance() string
	Plan(context.Context, json.RawMessage, CallContext) (Plan, error)
	Execute(context.Context, Plan, CallContext) (Result, error)
}

type toolUnwrapper interface{ UnwrapTool() Tool }

func unwrapTool(t Tool) Tool {
	for {
		wrapper, ok := t.(toolUnwrapper)
		if !ok {
			return t
		}
		next := wrapper.UnwrapTool()
		if next == nil || next == t {
			return t
		}
		t = next
	}
}

func NewPlan(toolID string, input json.RawMessage, requests []permission.Request, review json.RawMessage, data any) (Plan, error) {
	canonical, err := CanonicalJSON(input)
	if err != nil {
		return Plan{}, err
	}
	return Plan{ToolID: toolID, CanonicalInput: canonical, Permissions: requests, Review: review, Data: data}, nil
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

// Without returns a new Snapshot with the specified tool IDs removed.
// It returns the same Snapshot when the blacklist is empty.
func (s Snapshot) Without(blacklisted []string) Snapshot {
	if len(blacklisted) == 0 {
		return s
	}
	blacklist := make(map[string]bool, len(blacklisted))
	for _, id := range blacklisted {
		blacklist[id] = true
	}
	tools := make(map[string]Tool, len(s.tools))
	for id, t := range s.tools {
		if !blacklist[id] {
			tools[id] = t
		}
	}
	return Snapshot{tools: tools, definitions: definitions(tools)}
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

// SystemPromptGuidance collects the non-empty guidance text declared by every
// tool. Entries are sorted so the output is deterministic. It returns "" when
// no tool declares guidance, so the system context source is omitted entirely.
func (s Snapshot) SystemPromptGuidance() string {
	var entries []string
	for _, t := range s.tools {
		if g := t.SystemPromptGuidance(); g != "" {
			entries = append(entries, g)
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n\n")
}

func definitions(tools map[string]Tool) []Definition {
	out := make([]Definition, 0, len(tools))
	for _, t := range tools {
		out = append(out, Definition{ID: t.ID(), Description: t.Description(), Schema: append(json.RawMessage(nil), t.JSONSchema()...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Authorizer resolves the permission request produced while planning a tool
// call. The executor uses it before invoking the tool's Execute method.
type Authorizer interface {
	Authorize(context.Context, permission.Request) (permission.Decision, error)
}

type Executor struct {
	Snapshot       Snapshot
	Permissions    Authorizer
	ErrorAdvisor   ErrorAdvisor
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
		return Result{}, e.advise(ctx, err, raw, t)
	}
	if p.ToolID != id {
		return Result{}, errors.New("plan tool ID mismatch")
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
			p.Permissions[i].Description = description
			p.Permissions[i].Choices = ChoicesFor(t)
			p.Permissions[i].CanonicalInput = p.CanonicalInput
		}
	}
	for _, request := range p.Permissions {
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
		return Result{}, e.advise(ctx, err, raw, t)
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
	// responsibility. Spilling is the exception: the executor wrote the file and
	// is the only party holding its path, so it replaces the model copy to report
	// what it did. Without this the model receives a preview it cannot act on.
	if len(result.Text) > max && call.Outputs != nil {
		if result.Metadata == nil {
			result.Metadata = make(map[string]any)
		}
		stored, storeErr := call.Outputs.Store(ctx, call.SessionID, strings.NewReader(result.Text))
		if storeErr != nil {
			result.Metadata["output_lossy"] = true
			result.ModelText = modelText(result.ModelText) +
				fmt.Sprintf("\n... output exceeded %d bytes and could not be stored; the remainder is unrecoverable ...", max)
		} else {
			result.Metadata["output_path"] = stored.Path
			result.Metadata["output_bytes"] = stored.Size
			result.ModelText = modelText(stored.Preview) +
				fmt.Sprintf("\n... %d bytes total; full output saved to %s ...", stored.Size, stored.Path)
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

func (e Executor) advise(ctx context.Context, err error, raw json.RawMessage, target Tool) error {
	if e.ErrorAdvisor == nil {
		return err
	}
	provider, ok := unwrapTool(target).(ErrorAdviceProvider)
	if !ok {
		return err
	}
	advice, adviceErr := provider.ErrorAdvice(raw)
	if adviceErr != nil {
		return err
	}
	return e.ErrorAdvisor.Advise(ctx, err, advice)
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
	if _, err := CanonicalJSON(raw); err != nil {
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
