package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/formatter"
	"github.com/amirulashraf/parrot-coder/internal/lsp"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

func readPhase9File(path string, max int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("tool: workspace file exceeds byte limit")
	}
	return data, nil
}

type SkillTool struct{ Registry *skill.Registry }

func NewSkillTool(registry *skill.Registry) *SkillTool { return &SkillTool{Registry: registry} }
func (*SkillTool) ID() string                          { return "skill" }
func (t *SkillTool) Description() string {
	items := t.Registry.List()
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name+": "+item.Description)
	}
	if len(names) == 0 {
		return "Load the exact body and execution metadata of a discovered skill."
	}
	return "Load the exact body and execution metadata of a discovered skill. Available skills: " + strings.Join(names, "; ")
}
func (*SkillTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Load skill %q", input.Name), nil
}
func (*SkillTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)
}
func (t *SkillTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	loaded, err := t.Registry.Load(input.Name)
	if err != nil {
		return Plan{}, err
	}
	review, _ := json.Marshal(map[string]any{"name": loaded.Name, "description": loaded.Description, "agent": loaded.Agent, "model": loaded.Model, "allowed_tools": loaded.AllowedTools})
	return NewPlan(t.ID(), raw, nil, review, loaded)
}
func (t *SkillTool) Execute(_ context.Context, plan Plan, _ CallContext) (Result, error) {
	loaded, ok := plan.Data.(skill.Skill)
	if !ok {
		return Result{}, errors.New("skill: incompatible plan")
	}
	return Result{Text: loaded.Prompt, Metadata: map[string]any{"name": loaded.Name, "description": loaded.Description, "agent": loaded.Agent, "model": loaded.Model, "allowed_tools": loaded.AllowedTools}}, nil
}

type MCPCaller interface {
	CallTool(context.Context, string, json.RawMessage) (mcp.ToolResult, error)
}

type MCPTool struct {
	definition mcp.ToolDefinition
	caller     MCPCaller
	schema     json.RawMessage
}

func NewMCPTool(caller MCPCaller, definition mcp.ToolDefinition) (*MCPTool, error) {
	if caller == nil || definition.Name == "" || definition.Server == "" || definition.Tool == "" {
		return nil, errors.New("mcp tool: caller and complete definition are required")
	}
	var object map[string]any
	if err := json.Unmarshal(definition.InputSchema, &object); err != nil || object["type"] != "object" {
		return nil, fmt.Errorf("mcp tool %s: input schema must be an object schema", definition.Name)
	}
	if _, ok := object["additionalProperties"]; !ok {
		object["additionalProperties"] = false
	}
	schema, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	definition.InputSchema = append(json.RawMessage(nil), schema...)
	return &MCPTool{definition: definition, caller: caller, schema: schema}, nil
}
func (t *MCPTool) ID() string { return t.definition.Name }
func (t *MCPTool) Description() string {
	return fmt.Sprintf("MCP tool %s/%s. %s", t.definition.Server, t.definition.Tool, t.definition.Description)
}
func (t *MCPTool) DescribeRequest(json.RawMessage) (string, error) {
	return fmt.Sprintf("Call MCP tool %s/%s", t.definition.Server, t.definition.Tool), nil
}
func (t *MCPTool) JSONSchema() json.RawMessage { return append(json.RawMessage(nil), t.schema...) }
func (t *MCPTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	canonical, err := permission.CanonicalJSON(raw)
	if err != nil {
		return Plan{}, err
	}
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	review, _ := json.Marshal(map[string]any{"server": t.definition.Server, "tool": t.definition.Tool, "arguments_sha256": hash})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "mcp", Identifier: t.definition.Server + "/" + t.definition.Tool, Operation: "call", Attributes: map[string]string{"arguments_sha256": hash}}}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, mcpPlan{arguments: canonical, hash: hash})
}
func (t *MCPTool) Execute(ctx context.Context, plan Plan, _ CallContext) (Result, error) {
	planned, ok := plan.Data.(mcpPlan)
	if !ok {
		return Result{}, errors.New("mcp tool: incompatible plan")
	}
	digest := sha256.Sum256(planned.arguments)
	if hex.EncodeToString(digest[:]) != planned.hash {
		return Result{}, errors.New("mcp tool: arguments changed after planning")
	}
	result, err := t.caller.CallTool(ctx, t.definition.Name, planned.arguments)
	if err != nil {
		return Result{}, err
	}
	var text strings.Builder
	types := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		types = append(types, content.Type)
		if content.Type == "text" && content.Text != "" {
			if text.Len() != 0 {
				text.WriteByte('\n')
			}
			text.WriteString(content.Text)
		}
	}
	metadata := map[string]any{"server": t.definition.Server, "tool": t.definition.Tool, "content_types": types, "truncated": result.Truncated}
	if len(result.StructuredContent) != 0 {
		metadata["structured_content"] = json.RawMessage(append([]byte(nil), result.StructuredContent...))
	}
	return Result{Text: text.String(), Metadata: metadata}, nil
}

type mcpPlan struct {
	arguments json.RawMessage
	hash      string
}

type WebFetchTool struct{ Service *webfetch.Service }

func NewWebFetchTool(service *webfetch.Service) *WebFetchTool { return &WebFetchTool{Service: service} }
func (*WebFetchTool) ID() string                              { return "web_fetch" }
func (*WebFetchTool) Description() string {
	return "Fetch bounded HTTP or HTTPS text with GET or HEAD after exact network permission review."
}
func (*WebFetchTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input webFetchInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	normalized, err := normalizeFetch(input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Fetch %s with %s", normalized.URL, normalized.Method), nil
}
func (*WebFetchTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"}},"required":["url"],"additionalProperties":false}`)
}

type webFetchInput struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

func (t *WebFetchTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	if t.Service == nil {
		return Plan{}, errors.New("web_fetch: service is required")
	}
	var input webFetchInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	normalized, err := normalizeFetch(input)
	if err != nil {
		return Plan{}, err
	}
	review, _ := json.Marshal(webfetch.PermissionReview{URL: normalized.URL, Method: normalized.Method})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "network", Identifier: normalized.URL, Operation: normalized.Method}}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, webFetchPlan{Input: input, Normalized: normalized})
}
func (t *WebFetchTool) Execute(ctx context.Context, plan Plan, _ CallContext) (Result, error) {
	planned, ok := plan.Data.(webFetchPlan)
	if !ok {
		return Result{}, errors.New("web_fetch: incompatible plan")
	}
	revalidated, err := normalizeFetch(planned.Input)
	if err != nil || revalidated != planned.Normalized {
		return Result{}, errors.New("web_fetch: request changed after planning")
	}
	response, err := t.Service.Fetch(ctx, webfetch.Request{URL: planned.Normalized.URL, Method: planned.Normalized.Method})
	if err != nil {
		return Result{}, err
	}
	return Result{Text: response.Text, Metadata: map[string]any{"final_url": response.FinalURL, "status": response.Status, "content_type": response.ContentType, "truncated": response.Truncated}}, nil
}

type webFetchPlan struct {
	Input      webFetchInput
	Normalized webFetchInput
}

func normalizeFetch(input webFetchInput) (webFetchInput, error) {
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return webFetchInput{}, errors.New("web_fetch: only GET and HEAD are supported")
	}
	raw := strings.TrimSpace(input.URL)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return webFetchInput{}, errors.New("web_fetch: URL must be absolute HTTP or HTTPS without user information")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return webFetchInput{URL: parsed.String(), Method: method}, nil
}

type LSPClient interface {
	DidOpen(context.Context, string, string, string) error
	DidChange(context.Context, string, string) error
	DocumentVersion(string) (int, bool)
	Definition(context.Context, string, lsp.Position) ([]lsp.Location, error)
	References(context.Context, string, lsp.Position, bool) ([]lsp.Location, error)
	Hover(context.Context, string, lsp.Position) (*lsp.Hover, error)
	DocumentSymbols(context.Context, string) ([]lsp.SymbolInformation, error)
	Symbols(context.Context, string) ([]lsp.SymbolInformation, error)
	Diagnostics(lsp.DocumentURI) []lsp.Diagnostic
}

type LSPClientFunc func(context.Context, string) (LSPClient, error)

type LSPToolConfig struct {
	Client    LSPClientFunc
	Languages map[string]map[string]string
}

type LSPTool struct {
	kind   string
	config LSPToolConfig
}

func NewLSPTools(config LSPToolConfig) []Tool {
	result := make([]Tool, 0, 5)
	for _, kind := range []string{"diagnostics", "definition", "references", "hover", "symbols"} {
		result = append(result, &LSPTool{kind: kind, config: config})
	}
	return result
}
func (t *LSPTool) ID() string { return "lsp_" + t.kind }
func (t *LSPTool) Description() string {
	return "Read-only Language Server Protocol " + t.kind + " query within the workspace."
}
func (t *LSPTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input lspInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	target := input.Path
	if target == "" {
		target = input.Query
	}
	return fmt.Sprintf("Run LSP %s query with %q on %q", t.kind, input.Server, target), nil
}
func (t *LSPTool) JSONSchema() json.RawMessage {
	switch t.kind {
	case "diagnostics":
		return json.RawMessage(`{"type":"object","properties":{"server":{"type":"string"},"path":{"type":"string"}},"required":["server","path"],"additionalProperties":false}`)
	case "symbols":
		return json.RawMessage(`{"type":"object","properties":{"server":{"type":"string"},"path":{"type":"string"},"query":{"type":"string"}},"required":["server"],"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{"server":{"type":"string"},"path":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["server","path","line","character"],"additionalProperties":false}`)
	}
}

type lspInput struct {
	Server    string `json:"server"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Query     string `json:"query"`
}
type lspPlan struct {
	Input lspInput
	Path  string
	Text  string
	Lang  string
}

func (t *LSPTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.config.Client == nil || call.Workspace == nil {
		return Plan{}, errors.New("lsp: client and workspace are required")
	}
	var input lspInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Server == "" || input.Line < 0 || input.Character < 0 {
		return Plan{}, errors.New("lsp: server and nonnegative positions are required")
	}
	planned := lspPlan{Input: input}
	if input.Path != "" {
		path, err := call.Workspace.ResolveRead(input.Path)
		if err != nil {
			return Plan{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Plan{}, err
		}
		planned.Path, planned.Text = path, string(data)
		planned.Lang = t.language(input.Server, path)
	} else if t.kind != "symbols" || input.Query == "" {
		return Plan{}, errors.New("lsp: path is required (or query for workspace symbols)")
	}
	return NewPlan(t.ID(), raw, nil, nil, planned)
}
func (t *LSPTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	planned, ok := plan.Data.(lspPlan)
	if !ok {
		return Result{}, errors.New("lsp: incompatible plan")
	}
	if planned.Path != "" {
		path, err := call.Workspace.ResolveRead(planned.Input.Path)
		if err != nil || path != planned.Path {
			return Result{}, errors.New("lsp: path changed after planning")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != planned.Text {
			return Result{}, errors.New("lsp: file changed after planning")
		}
	}
	client, err := t.config.Client(ctx, planned.Input.Server)
	if err != nil {
		return Result{}, err
	}
	if planned.Path != "" {
		if _, open := client.DocumentVersion(planned.Path); !open {
			if err := client.DidOpen(ctx, planned.Path, planned.Lang, planned.Text); err != nil {
				return Result{}, err
			}
		} else if err := client.DidChange(ctx, planned.Path, planned.Text); err != nil {
			return Result{}, err
		}
	}
	position := lsp.Position{Line: planned.Input.Line, Character: planned.Input.Character}
	var value any
	switch t.kind {
	case "diagnostics":
		uri, err := lsp.FileURI(call.Workspace.Root(), planned.Path)
		if err != nil {
			return Result{}, err
		}
		value = client.Diagnostics(uri)
	case "definition":
		value, err = client.Definition(ctx, planned.Path, position)
	case "references":
		value, err = client.References(ctx, planned.Path, position, true)
	case "hover":
		value, err = client.Hover(ctx, planned.Path, position)
	case "symbols":
		if planned.Path != "" {
			value, err = client.DocumentSymbols(ctx, planned.Path)
		} else {
			value, err = client.Symbols(ctx, planned.Input.Query)
		}
	}
	if err != nil {
		return Result{}, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: string(encoded), Metadata: map[string]any{"server": planned.Input.Server, "path": planned.Input.Path, "kind": t.kind}}, nil
}
func (t *LSPTool) language(server, path string) string {
	extension := strings.ToLower(filepath.Ext(path))
	if mapping := t.config.Languages[server]; mapping != nil {
		if language := mapping[extension]; language != "" {
			return language
		}
		return mapping[strings.TrimPrefix(extension, ".")]
	}
	return strings.TrimPrefix(extension, ".")
}

type FormatTool struct {
	Formatters *formatter.Registry
	Changes    *change.Service
	Snapshots  *snapshot.Service
}

func NewFormatTool(formatters *formatter.Registry, changes *change.Service, snapshots *snapshot.Service) *FormatTool {
	return &FormatTool{Formatters: formatters, Changes: changes, Snapshots: snapshots}
}
func (*FormatTool) ID() string { return "format" }
func (*FormatTool) Description() string {
	return "Run the configured formatter during planning, review its command and exact proposed diff, then commit those bytes without rerunning it."
}
func (*FormatTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Format workspace file %q", input.Path), nil
}
func (*FormatTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["path","expected_sha256"],"additionalProperties":false}`)
}
func (t *FormatTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Formatters == nil || call.Workspace == nil {
		return Plan{}, errors.New("format: formatter registry and workspace are required")
	}
	var input struct {
		Path           string `json:"path"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	formatterPlan, err := t.Formatters.Plan(input.Path, input.ExpectedSHA256)
	if err != nil {
		return Plan{}, err
	}
	formatted, err := t.Formatters.Format(ctx, formatterPlan)
	if err != nil {
		return Plan{}, err
	}
	commandJSON, _ := json.Marshal(formatterPlan.Command)
	commandDigest := sha256.Sum256(commandJSON)
	commandHash := hex.EncodeToString(commandDigest[:])
	if !formatted.Changed {
		review, _ := json.Marshal(map[string]any{"path": formatted.Path, "formatter": formatterPlan.Formatter, "command": formatterPlan.Command, "command_sha256": commandHash, "before_sha256": formatted.BeforeHash, "after_sha256": formatted.AfterHash, "diff": ""})
		return NewPlan(t.ID(), raw, nil, review, formatNoop{Path: formatted.Path, Hash: formatted.BeforeHash, Formatter: formatterPlan.Formatter})
	}
	path, err := call.Workspace.ResolveRead(input.Path)
	if err != nil || path != formatted.Path {
		return Plan{}, errors.New("format: path changed while preparing proposal")
	}
	before, err := os.ReadFile(path)
	if err != nil || change.SHA256(before) != formatted.BeforeHash {
		return Plan{}, change.ErrStale
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Plan{}, errors.New("format: target must be a regular file")
	}
	beforeState := change.FileState{Path: path, Exists: true, Mode: info.Mode().Perm(), Data: append([]byte(nil), before...), SHA256: formatted.BeforeHash}
	afterState := change.FileState{Path: path, Exists: true, Mode: info.Mode().Perm(), Data: append([]byte(nil), formatted.Proposed...), SHA256: formatted.AfterHash}
	changePlan := change.Plan{Mutations: []change.Mutation{{RequestedPath: input.Path, Path: path, Before: beforeState, After: afterState}}, Diff: formatted.Diff}
	review, _ := json.Marshal(map[string]any{"path": path, "formatter": formatterPlan.Formatter, "command": formatterPlan.Command, "command_sha256": commandHash, "before_sha256": formatted.BeforeHash, "after_sha256": formatted.AfterHash, "diff": formatted.Diff})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "filesystem", Identifier: path, Operation: "write", Attributes: map[string]string{"before_sha256": formatted.BeforeHash, "after_sha256": formatted.AfterHash}}}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, mutationPlan{changePlan})
}
func (t *FormatTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if noop, ok := plan.Data.(formatNoop); ok {
		path, err := call.Workspace.ResolveRead(noop.Path)
		if err != nil || path != noop.Path {
			return Result{}, change.ErrStale
		}
		data, err := os.ReadFile(path)
		if err != nil || change.SHA256(data) != noop.Hash {
			return Result{}, change.ErrStale
		}
		return Result{Text: "File is already formatted.", Metadata: map[string]any{"path": noop.Path, "formatter": noop.Formatter, "changed": false}}, nil
	}
	return executeMutation(ctx, t.Changes, t.Snapshots, plan, call)
}

type formatNoop struct {
	Path      string
	Hash      string
	Formatter string
}

type AgentLookup func(string) (bool, error)

const maxGitDiffBytes = 4 << 20

// GitDiffTool exposes only fixed, read-only Git operations. Review workers use
// it to inspect changes without receiving the general shell tool.
type GitDiffTool struct{}

func NewGitDiffTool() Tool        { return &GitDiffTool{} }
func (t *GitDiffTool) ID() string { return "git_diff" }
func (t *GitDiffTool) Description() string {
	return "Read a bounded Git diff for uncommitted changes, a base branch, or a commit. Uncommitted output also lists untracked paths."
}
func (t *GitDiffTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input gitDiffInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	if input.Target == "" {
		input.Target = "uncommitted"
	}
	return "Read Git diff for " + input.Target, nil
}
func (t *GitDiffTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","enum":["uncommitted","base","commit"]},"ref":{"type":"string","description":"Required branch or commit when target is base or commit."}},"additionalProperties":false}`)
}

type gitDiffInput struct {
	Target string `json:"target"`
	Ref    string `json:"ref"`
}

func (t *GitDiffTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("git_diff: workspace is required")
	}
	var input gitDiffInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Target == "" {
		input.Target = "uncommitted"
	}
	if input.Target != "uncommitted" && input.Target != "base" && input.Target != "commit" {
		return Plan{}, errors.New("git_diff: target must be uncommitted, base, or commit")
	}
	input.Ref = strings.TrimSpace(input.Ref)
	if input.Target == "uncommitted" {
		if input.Ref != "" {
			return Plan{}, errors.New("git_diff: ref is not valid for uncommitted changes")
		}
	} else if !validGitRef(input.Ref) {
		return Plan{}, errors.New("git_diff: a valid ref is required for base or commit")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *GitDiffTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(gitDiffInput)
	if !ok || call.Workspace == nil {
		return Result{}, errors.New("git_diff: incompatible plan or missing workspace")
	}
	root := call.Workspace.Root()
	var output string
	var truncated bool
	var err error
	switch input.Target {
	case "uncommitted":
		// Read index and worktree diffs separately so unborn repositories do not
		// fail merely because HEAD does not exist.
		output, truncated, err = runBoundedGit(ctx, root, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--find-renames", "--")
		if err == nil {
			var unstaged string
			var unstagedTruncated bool
			unstaged, unstagedTruncated, err = runBoundedGit(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--")
			truncated = truncated || unstagedTruncated
			if unstaged != "" {
				output += "\n" + unstaged
			}
		}
		if err == nil {
			var status string
			var statusTruncated bool
			status, statusTruncated, err = runBoundedGit(ctx, root, "status", "--short", "--untracked-files=all")
			truncated = truncated || statusTruncated
			if err == nil && status != "" {
				output += "\n\nGit status (including untracked paths):\n" + status
			}
		}
	case "base":
		var base string
		base, truncated, err = runBoundedGit(ctx, root, "merge-base", "HEAD", input.Ref)
		if err == nil {
			base = strings.TrimSpace(base)
			var diffTruncated bool
			output, diffTruncated, err = runBoundedGit(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--find-renames", base, "--")
			truncated = truncated || diffTruncated
		}
	case "commit":
		output, truncated, err = runBoundedGit(ctx, root, "show", "--format=fuller", "--no-ext-diff", "--no-textconv", "--find-renames", input.Ref, "--")
	}
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(output) == "" {
		output = "No changes found."
	}
	if truncated {
		output += "\n\n[git_diff output truncated; narrow the review target before drawing conclusions.]"
	}
	return Result{Text: output, Metadata: map[string]any{"target": input.Target, "ref": input.Ref, "truncated": truncated}}, nil
}

func validGitRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return false
		}
	}
	return true
}

func runBoundedGit(ctx context.Context, root string, args ...string) (string, bool, error) {
	// Disable optional writes and repository-configured helper execution. In
	// particular, core.fsmonitor and diff textconv/external drivers must not turn
	// a read-only reviewer operation into arbitrary process execution.
	gitArgs := []string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "diff.external="}
	gitArgs = append(gitArgs, args...)
	command := exec.CommandContext(ctx, "git", gitArgs...)
	command.Dir = root
	var stdout strings.Builder
	var stderr strings.Builder
	stdoutWriter := &limitedStringWriter{builder: &stdout, remaining: maxGitDiffBytes}
	stderrWriter := &limitedStringWriter{builder: &stderr, remaining: 64 << 10}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if stderrWriter.truncated {
			message += " [stderr truncated]"
		}
		if message == "" {
			message = err.Error()
		}
		return "", false, fmt.Errorf("git_diff: git %s: %s", args[0], message)
	}
	return stdout.String(), stdoutWriter.truncated, nil
}

type limitedStringWriter struct {
	builder   *strings.Builder
	remaining int
	truncated bool
}

func (w *limitedStringWriter) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
		w.truncated = true
	}
	_, _ = w.builder.Write(p)
	w.remaining -= len(p)
	return original, nil
}

type TaskTool struct {
	Kind       string
	Manager    *subagent.Manager
	Agents     AgentLookup
	CancelWait time.Duration
}

func NewTaskTools(manager *subagent.Manager, agents AgentLookup) []Tool {
	return []Tool{
		&TaskTool{Kind: "task", Manager: manager, Agents: agents, CancelWait: 5 * time.Second},
		&TaskTool{Kind: "task_status", Manager: manager, Agents: agents},
		&TaskTool{Kind: "task_cancel", Manager: manager, Agents: agents, CancelWait: 5 * time.Second},
	}
}

const reviewAgentID = "review"

// ReviewTool starts the built-in, read-only review worker. It is deliberately
// separate from task so a parent model can request a review without selecting
// or knowing the implementation profile used by the child session.
type ReviewTool struct {
	Manager    *subagent.Manager
	Agents     AgentLookup
	CancelWait time.Duration
}

func NewReviewTool(manager *subagent.Manager, agents AgentLookup) Tool {
	return &ReviewTool{Manager: manager, Agents: agents, CancelWait: 5 * time.Second}
}

func (t *ReviewTool) ID() string { return "review" }
func (t *ReviewTool) Description() string {
	return "Launch the built-in read-only review subagent and wait for its actionable findings. Use after implementing changes or when explicitly asked to review code."
}
func (t *ReviewTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input reviewInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return "Launch read-only review subagent", nil
}
func (t *ReviewTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"The exact change or review target, including any additional review instructions."},"model":{"type":"string","description":"Optional model override for the reviewer."}},"required":["prompt"],"additionalProperties":false}`)
}

type reviewInput struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
}

func (t *ReviewTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Manager == nil || t.Agents == nil || call.SessionID == "" || call.Agent == "" {
		return Plan{}, errors.New("review: manager, agent registry, session, and caller agent are required")
	}
	var input reviewInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return Plan{}, errors.New("review: prompt is required")
	}
	if _, err := t.Agents(call.Agent); err != nil {
		return Plan{}, err
	}
	reviewerReadOnly, err := t.Agents(reviewAgentID)
	if err != nil {
		return Plan{}, err
	}
	if !reviewerReadOnly {
		return Plan{}, errors.New("review: built-in reviewer must be read-only")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *ReviewTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(reviewInput)
	if !ok {
		return Result{}, errors.New("review: incompatible plan")
	}
	id, err := t.Manager.Launch(call.SessionID, []string{call.Agent}, subagent.Request{
		Prompt: input.Prompt, Agent: reviewAgentID, Model: input.Model, ToolCallID: call.ToolCallID,
	})
	if err != nil {
		return Result{}, err
	}
	task, err := t.Manager.Await(ctx, call.SessionID, id)
	if ctx.Err() != nil {
		cancelWait := t.CancelWait
		if cancelWait <= 0 {
			cancelWait = 5 * time.Second
		}
		cancelCtx, cancel := context.WithTimeout(context.Background(), cancelWait)
		cancelErr := t.Manager.Cancel(cancelCtx, call.SessionID, id)
		cancel()
		if cancelErr == nil {
			_ = t.Manager.Forget(call.SessionID, id)
		}
		return Result{}, ctx.Err()
	}
	if err != nil {
		_ = t.Manager.Forget(call.SessionID, id)
		return Result{}, err
	}
	result := taskResult(task)
	if err := t.Manager.Forget(call.SessionID, id); err != nil {
		return Result{}, err
	}
	return result, nil
}
func (t *TaskTool) ID() string { return t.Kind }
func (t *TaskTool) Description() string {
	switch t.Kind {
	case "task":
		return "Launch a child agent in an isolated session and wait for its final output."
	case "task_status":
		return "Read the status of a child task owned by this session."
	default:
		return "Cancel a child task owned by this session."
	}
}
func (t *TaskTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input taskInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	switch t.Kind {
	case "task":
		return fmt.Sprintf("Launch %q subagent", input.Agent), nil
	case "task_status":
		return fmt.Sprintf("Read status of task %q", input.TaskID), nil
	default:
		return fmt.Sprintf("Cancel task %q", input.TaskID), nil
	}
}
func (t *TaskTool) JSONSchema() json.RawMessage {
	if t.Kind == "task" {
		return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string"}},"required":["prompt","agent"],"additionalProperties":false}`)
	}
	return json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`)
}

type taskInput struct {
	Prompt string `json:"prompt"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	TaskID string `json:"task_id"`
}

func (t *TaskTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Manager == nil || call.SessionID == "" || call.Agent == "" {
		return Plan{}, errors.New("task: manager, session, and caller agent are required")
	}
	var input taskInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if t.Kind == "task" {
		if strings.TrimSpace(input.Prompt) == "" || input.Agent == "" || t.Agents == nil {
			return Plan{}, errors.New("task: prompt, target agent, and agent registry are required")
		}
		callerReadOnly, err := t.Agents(call.Agent)
		if err != nil {
			return Plan{}, err
		}
		targetReadOnly, err := t.Agents(input.Agent)
		if err != nil {
			return Plan{}, err
		}
		if callerReadOnly && !targetReadOnly {
			return Plan{}, fmt.Errorf("task: read-only agent %q cannot delegate to writable agent %q", call.Agent, input.Agent)
		}
	} else if input.TaskID == "" {
		return Plan{}, errors.New("task: task_id is required")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *TaskTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(taskInput)
	if !ok {
		return Result{}, errors.New("task: incompatible plan")
	}
	if t.Kind == "task_status" {
		task, err := t.Manager.Status(call.SessionID, input.TaskID)
		return taskResult(task), err
	}
	if t.Kind == "task_cancel" {
		if err := t.Manager.Cancel(ctx, call.SessionID, input.TaskID); err != nil {
			return Result{}, err
		}
		task, err := t.Manager.Status(call.SessionID, input.TaskID)
		return taskResult(task), err
	}
	id, err := t.Manager.Launch(call.SessionID, []string{call.Agent}, subagent.Request{Prompt: input.Prompt, Agent: input.Agent, Model: input.Model, ToolCallID: call.ToolCallID})
	if err != nil {
		return Result{}, err
	}
	task, err := t.Manager.Await(ctx, call.SessionID, id)
	if ctx.Err() != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), t.CancelWait)
		_ = t.Manager.Cancel(cancelCtx, call.SessionID, id)
		cancel()
		return Result{}, ctx.Err()
	}
	if err != nil {
		return Result{}, err
	}
	return taskResult(task), nil
}
func taskResult(task subagent.Task) Result {
	metadata := map[string]any{"task_id": task.ID, "status": task.Status, "agent": task.Agent, "model": task.Model, "depth": task.Depth, "truncated": task.Truncated, "usage": task.Usage, "tool_uses": task.ToolUses}
	if task.Error != "" {
		metadata["error"] = task.Error
	}
	return Result{Text: task.Output, Metadata: metadata}
}

type Phase9Services struct {
	Skills     *skill.Registry
	MCP        MCPCaller
	MCPTools   []mcp.ToolDefinition
	WebFetch   *webfetch.Service
	LSP        LSPToolConfig
	Formatters *formatter.Registry
	Changes    *change.Service
	Snapshots  *snapshot.Service
	Subagents  *subagent.Manager
	Agents     AgentLookup
}

func RegisterPhase9(registry *Registry, services Phase9Services) error {
	if registry == nil || services.Skills == nil || services.WebFetch == nil || services.Subagents == nil || services.Agents == nil {
		return errors.New("tool: phase 9 core services are required")
	}
	items := []Tool{NewSkillTool(services.Skills), NewWebFetchTool(services.WebFetch), NewGitDiffTool()}
	if services.LSP.Client != nil {
		items = append(items, NewLSPTools(services.LSP)...)
	}
	if services.Formatters != nil {
		items = append(items, NewFormatTool(services.Formatters, services.Changes, services.Snapshots))
	}
	items = append(items, NewTaskTools(services.Subagents, services.Agents)...)
	items = append(items, NewReviewTool(services.Subagents, services.Agents))
	definitions := append([]mcp.ToolDefinition(nil), services.MCPTools...)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	for _, definition := range definitions {
		item, err := NewMCPTool(services.MCP, definition)
		if err != nil {
			return err
		}
		items = append(items, item)
	}
	for _, item := range items {
		if err := registry.Register(item); err != nil {
			return fmt.Errorf("tool: register %s: %w", item.ID(), err)
		}
	}
	return nil
}
