package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/lsp"
	"os"
	"path/filepath"
	"strings"
)

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
	text := string(encoded)
	// An LSP response is a single encoded document, so an oversized one is
	// replaced rather than cut: truncating it would not parse.
	model := text
	if len(model) > maxModelTextBytes {
		model = fmt.Sprintf(`{"truncated":true,"kind":%q,"bytes":%d,"message":"response too large for the model context; narrow the query or request a specific path"}`, t.kind, len(text))
	}
	return Result{Text: text, ModelText: model, Metadata: map[string]any{"server": planned.Input.Server, "path": planned.Input.Path, "kind": t.kind}}, nil
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
