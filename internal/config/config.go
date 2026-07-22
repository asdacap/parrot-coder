// Package config discovers, parses, and merges Parrot configuration files.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	FileName           = "parrot.yaml"
	PredefinedFileName = "predefined_config.yaml"
	maxConfigBytes     = 4 << 20
)

// Config is the typed configuration consumed by later application phases.
type Config struct {
	DefaultModel  string              `json:"model,omitempty"`
	Providers     map[string]Provider `json:"providers,omitempty"`
	MCP           map[string]MCP      `json:"mcp,omitempty"`
	LSP           map[string]LSP      `json:"lsp,omitempty"`
	WebFetch      WebFetch            `json:"web_fetch,omitempty"`
	ToolBlacklist []string            `json:"tool_blacklist,omitempty"`
	SandboxRules  []SandboxRule       `json:"sandbox_rules,omitempty"`
}

// SandboxRule is one ordered filesystem rule applied to the sandbox. Rule
// is one of: allow_write, deny_read, allow_read, deny_write.
type SandboxRule struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

type MCP struct {
	Transport              string            `json:"transport"`
	Command                string            `json:"command,omitempty"`
	Args                   []string          `json:"args,omitempty"`
	Env                    map[string]string `json:"env,omitempty"`
	CWD                    string            `json:"cwd,omitempty"`
	URL                    string            `json:"url,omitempty"`
	Headers                map[string]string `json:"headers,omitempty"`
	Enabled                bool              `json:"enabled"`
	AllowInsecureLocalhost bool              `json:"allow_insecure_localhost,omitempty"`
	StartupTimeoutMS       int               `json:"startup_timeout_ms,omitempty"`
	CallTimeoutMS          int               `json:"call_timeout_ms,omitempty"`
}

type LSP struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Extensions []string          `json:"extensions,omitempty"`
	Languages  map[string]string `json:"languages,omitempty"`
	TimeoutMS  int               `json:"timeout_ms,omitempty"`
}

type WebFetch struct {
	AllowPrivate bool `json:"allow_private,omitempty"`
}

// Provider describes an OpenAI-compatible provider and its known models.
// APIKeyEnv names an environment variable; configuration files should not
// contain credential values.
type Provider struct {
	Type                   string            `json:"type,omitempty"`
	Protocol               string            `json:"protocol,omitempty"`
	BaseURL                string            `json:"base_url,omitempty"`
	APIKeyEnv              string            `json:"api_key_env,omitempty"`
	Headers                map[string]string `json:"headers,omitempty"`
	AllowInsecureLocalhost bool              `json:"allow_insecure_localhost,omitempty"`
	HeaderTimeoutMS        *int              `json:"header_timeout_ms,omitempty"`
	// ProviderPreferences is an opaque JSON object forwarded as the
	// top-level "provider" field of each request body. OpenAI-compatible
	// routers such as OpenRouter use it to steer routing, fallback, and
	// data-collection behavior (e.g. order, allow_fallbacks, only, ignore,
	// sort). The blob is validated as a JSON object at request time, so any
	// vendor's schema is accepted without coupling config to it.
	ProviderPreferences json.RawMessage  `json:"provider_preferences,omitempty"`
	Models              map[string]Model `json:"models,omitempty"`
}

// Model contains provider-specific model metadata needed for selection.
type Model struct {
	Name      string             `json:"name,omitempty"`
	Context   int                `json:"context,omitempty"`
	MaxTokens int                `json:"max_tokens,omitempty"`
	Tools     bool               `json:"tools,omitempty"`
	Reasoning bool               `json:"reasoning,omitempty"`
	Output    []string           `json:"output,omitempty"`
	Variants  map[string]Variant `json:"variants,omitempty"`
}

// Variant describes provider request options exposed under a stable name.
type Variant struct {
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// SourceKind identifies a configuration file's scope.
type SourceKind string

const (
	SourceGlobal  SourceKind = "global"
	SourceProject SourceKind = "project"
)

// Source records a loaded file in low-to-high precedence order.
type Source struct {
	Path string
	Kind SourceKind
}

// Options defines config discovery boundaries. ConfigDir is Parrot's
// application config directory, not XDG_CONFIG_HOME itself.
type Options struct {
	ConfigDir   string
	ProjectRoot string
	CWD         string
}

// Result contains merged typed configuration and its provenance. Provenance
// maps dotted JSON field paths to the last file that assigned them.
type Result struct {
	Config     Config
	Sources    []Source
	Provenance map[string]string
}

// Discover returns existing config files in deterministic low-to-high
// precedence order: global, then each directory from project root to cwd.
// Within one directory, .parrot/parrot.yaml has higher precedence.
func Discover(options Options) ([]Source, error) {
	root, cwd, err := discoveryBounds(options.ProjectRoot, options.CWD)
	if err != nil {
		return nil, err
	}

	candidates := []Source{{Path: filepath.Join(options.ConfigDir, FileName), Kind: SourceGlobal}}
	relative, _ := filepath.Rel(root, cwd)
	dir := root
	parts := []string(nil)
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	for depth := 0; depth <= len(parts); depth++ {
		candidates = append(candidates,
			Source{Path: filepath.Join(dir, FileName), Kind: SourceProject},
			Source{Path: filepath.Join(dir, ".parrot", FileName), Kind: SourceProject},
		)
		if depth < len(parts) {
			dir = filepath.Join(dir, parts[depth])
		}
	}

	result := make([]Source, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := os.Stat(candidate.Path)
		switch {
		case err == nil && !info.IsDir():
			candidate.Path, err = filepath.Abs(candidate.Path)
			if err != nil {
				return nil, fmt.Errorf("resolve config path: %w", err)
			}
			result = append(result, candidate)
		case err == nil:
			return nil, fmt.Errorf("config path %q is a directory", candidate.Path)
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return nil, fmt.Errorf("inspect config %q: %w", candidate.Path, err)
		}
	}
	return result, nil
}

func discoveryBounds(root, cwd string) (string, string, error) {
	if root == "" {
		return "", "", errors.New("project root is required")
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("get working directory: %w", err)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve project root: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", "", fmt.Errorf("resolve working directory: %w", err)
	}
	root, cwd = filepath.Clean(root), filepath.Clean(cwd)
	relative, err := filepath.Rel(root, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("working directory %q is outside project root %q", cwd, root)
	}
	return root, cwd, nil
}

// Load discovers and merges all applicable files. If no file exists and a
// ConfigDir is provided, a commented starter template is written there first.
func Load(options Options) (Result, error) {
	sources, err := Discover(options)
	if err != nil {
		return Result{}, err
	}
	if len(sources) == 0 && options.ConfigDir != "" {
		globalPath := filepath.Join(options.ConfigDir, FileName)
		if err := writeDefaultConfig(globalPath); err != nil {
			return Result{}, fmt.Errorf("write default config: %w", err)
		}
		absPath, err := filepath.Abs(globalPath)
		if err != nil {
			return Result{}, fmt.Errorf("resolve default config path: %w", err)
		}
		sources = []Source{{Path: absPath, Kind: SourceGlobal}}
	}
	merged := make(map[string]any)
	provenance := make(map[string]string)
	for _, source := range sources {
		data, err := readBoundedFile(source.Path, maxConfigBytes)
		if err != nil {
			return Result{}, fmt.Errorf("read config %q: %w", source.Path, err)
		}
		value, err := Parse(data)
		if err != nil {
			return Result{}, fmt.Errorf("parse config %q: %w", source.Path, err)
		}
		mergeObject(merged, value, "", source.Path, provenance)
	}
	// Snapshot configuration is obsolete, but accepting it keeps existing
	// configuration files usable after filesystem journaling was removed.
	delete(merged, "snapshot")

	data, err := json.Marshal(merged)
	if err != nil {
		return Result{}, fmt.Errorf("encode merged config: %w", err)
	}
	var typed Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&typed); err != nil {
		return Result{}, fmt.Errorf("decode merged config: %w", err)
	}
	if typed.Providers == nil {
		typed.Providers = make(map[string]Provider)
	}
	if typed.MCP == nil {
		typed.MCP = make(map[string]MCP)
	}
	if typed.LSP == nil {
		typed.LSP = make(map[string]LSP)
	}
	if options.ConfigDir != "" {
		if err := WritePredefinedConfig(filepath.Join(options.ConfigDir, PredefinedFileName)); err != nil {
			return Result{}, fmt.Errorf("write predefined config: %w", err)
		}
	}
	return Result{Config: typed, Sources: sources, Provenance: provenance}, nil
}

func readBoundedFile(path string, max int64) ([]byte, error) {
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
		return nil, fmt.Errorf("config exceeds %d bytes", max)
	}
	return data, nil
}

// Parse converts one YAML object into generic JSON values and rejects
// duplicate object keys at every depth.
func Parse(data []byte) (map[string]any, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if err := rejectDuplicateYAMLKeys(&document, "$"); err != nil {
		return nil, err
	}
	var object map[string]any
	if err := document.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("config root must be a YAML object")
	}
	return object, nil
}

func rejectDuplicateYAMLKeys(node *yaml.Node, path string) error {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return nil
		}
		return rejectDuplicateYAMLKeys(node.Content[0], path)
	case yaml.MappingNode:
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			if keyNode.Kind != yaml.ScalarNode {
				return fmt.Errorf("non-scalar key at %s", path)
			}
			key := keyNode.Value
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if seen[key] {
				return fmt.Errorf("duplicate object key %q at %s", key, path)
			}
			seen[key] = true
			if err := rejectDuplicateYAMLKeys(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index := 0; index < len(node.Content); index++ {
			if err := rejectDuplicateYAMLKeys(node.Content[index], fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeObject(destination, source map[string]any, prefix, sourcePath string, provenance map[string]string) {
	for key, incoming := range source {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		incomingObject, incomingIsObject := incoming.(map[string]any)
		existingObject, existingIsObject := destination[key].(map[string]any)
		if incomingIsObject && existingIsObject {
			mergeObject(existingObject, incomingObject, path, sourcePath, provenance)
			continue
		}
		for recordedPath := range provenance {
			if recordedPath == path || strings.HasPrefix(recordedPath, path+".") {
				delete(provenance, recordedPath)
			}
		}
		destination[key] = incoming
		markProvenance(incoming, path, sourcePath, provenance)
	}
}

func markProvenance(value any, path, source string, provenance map[string]string) {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		provenance[path] = source
		return
	}
	for key, child := range object {
		markProvenance(child, path+"."+key, source, provenance)
	}
}

// defaultConfigYAML is a commented starter template. Example values are
// commented out so the file is safe to parse; only web_fetch carries the
// safe default value so the document root remains a valid YAML object.
// predefinedConfigYAML is an always-overwritten, fully uncommented reference YAML
// file that documents every default configuration field. The agent reads this
// file programmatically to determine default values instead of relying on
// hardcoded defaults elsewhere. The file is regenerated at every startup.
const predefinedConfigYAML = `# Predefined configuration reference.
# This file is regenerated at every startup and documents all configurable
# fields with their default values. The agent reads this file to determine
# default configuration values.

# Default model selected as provider/model.
model: ""

# Tool blacklist: tools listed here are disabled and not available to the model.
# tool_blacklist:
#   - unrestricted_shell

# OpenAI-compatible providers and their model catalogs.
# providers:
#   provider:
#     # Provider adapter type: 'compatible' or 'openai-compatible'.
#     type: openai-compatible
#     # API protocol: 'responses' or 'chat-completions'.
#     protocol: responses
#     # Provider base URL, e.g. https://api.example.test/v1.
#     base_url: https://api.example.test/v1
#     # Environment variable holding the API key.
#     api_key_env: PROVIDER_API_KEY
#     # Extra headers to send. Do not store secrets here.
#     headers:
#       X-Tenant: non-secret-value
#     # Allow plain HTTP for localhost endpoints.
#     allow_insecure_localhost: false
#     # Milliseconds to wait for response headers; zero disables.
#     header_timeout_ms: 10000
#     # OpenRouter-style provider routing preferences.
#     provider_preferences:
#       order:
#         - anthropic
#       allow_fallbacks: false
#       sort: price
#     # Model metadata not available from the endpoint catalog.
#     models:
#       model:
#         # Display name for the model.
#         name: Display Name
#         # Context window in tokens.
#         context: 128000
#         # Maximum output tokens.
#         max_tokens: 16384
#         # Whether the model supports tool calls.
#         tools: true
#         # Whether the model supports reasoning tokens.
#         reasoning: false
#         # Supported output modalities, e.g. text.
#         output:
#           - text
#         # Named provider request options.
#         variants:
#           high:
#             # Reasoning effort passed to the provider, e.g. low/medium/high.
#             reasoning_effort: high

# Model Context Protocol servers to start.
# mcp:
#   server-name:
#     # Transport: 'stdio' or 'http'.
#     transport: stdio
#     # Absolute path to the server executable for stdio.
#     command: /absolute/path/to/server
#     # Arguments for the server command.
#     args:
#       - --stdio
#     # Environment variables for the server process.
#     env:
#       NAME: value
#     # Working directory for the server process.
#     cwd: /absolute/working/directory
#     # Endpoint URL for http transport.
#     url: https://mcp.example.test/rpc
#     # Extra headers for http requests. Do not store secrets here.
#     headers:
#       X-Tenant: non-secret-value
#     # Whether the server is started.
#     enabled: false
#     # Allow plain HTTP for localhost endpoints.
#     allow_insecure_localhost: false
#     # Milliseconds to wait for the server to start; zero uses default.
#     startup_timeout_ms: 15000
#     # Milliseconds to wait for a call; zero uses default.
#     call_timeout_ms: 30000

# Language servers for the workspace.
# lsp:
#   go:
#     # Absolute path to the language server executable.
#     command: /absolute/path/to/gopls
#     # Arguments for the language server.
#     args:
#       - serve
#     # Environment variables for the language server.
#     env:
#       GOTOOLCHAIN: local
#     # File extensions associated with this server.
#     extensions:
#       - .go
#     # Maps file extensions to language IDs.
#     languages:
#       .go: go
#     # Milliseconds to wait for responses; zero uses default.
#     timeout_ms: 15000

# Ordered sandbox rules applied after base workspace mounts. Each rule
# has a path and an action: allow_write, deny_read, allow_read, or
# deny_write. Later rules override earlier ones. Requires global
# configuration.
# sandbox_rules:
#   - path: /absolute/path/to/dir
#     rule: allow_write
#   - path: /another/path
#     rule: deny_read

# Web fetch restrictions.
web_fetch:
  # Allow fetching from private addresses; increases SSRF risk.
  allow_private: false
`

// WritePredefinedConfig writes the predefined configuration reference YAML to
// the specified path. The file is always overwritten; it documents every default
// configuration field for the agent to read programmatically.
func WritePredefinedConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(predefinedConfigYAML), 0o600)
}

const defaultConfigYAML = `# Parrot Coder configuration file.
# Uncomment and edit the sections below to configure Parrot.

# Default model selected as provider/model.
# model: provider/model

# Tool blacklist: tools listed here are disabled and not available to the model.
# tool_blacklist:
#   - unrestricted_shell

# OpenAI-compatible providers and their model catalogs.
# providers:
#   provider:
#     # Provider adapter type: 'compatible' or 'openai-compatible'.
#     type: openai-compatible
#     # API protocol: 'responses' or 'chat-completions'.
#     protocol: responses
#     # Provider base URL, e.g. https://api.example.test/v1.
#     base_url: https://api.example.test/v1
#     # Environment variable holding the API key.
#     api_key_env: PROVIDER_API_KEY
#     # Extra headers to send. Do not store secrets here.
#     headers:
#       X-Tenant: non-secret-value
#     # Allow plain HTTP for localhost endpoints.
#     allow_insecure_localhost: false
#     # Milliseconds to wait for response headers; zero disables.
#     header_timeout_ms: 10000
#     # OpenRouter-style provider routing preferences, forwarded as the
#     # top-level "provider" object of each request body. Only the openrouter
#     # provider reads this; it is ignored by other providers. See
#     # https://openrouter.ai/docs/api-reference/parameters for the schema.
#     provider_preferences:
#       # Provider slugs to try in order, e.g. ["anthropic", "openai"].
#       order:
#         - anthropic
#       # Whether to allow backup providers when the primary is unavailable.
#       allow_fallbacks: false
#       # Sort providers by price, throughput, or latency.
#       sort: price
#     # Model metadata not available from the endpoint catalog.
#     models:
#       model:
#         # Display name for the model.
#         name: Display Name
#         # Context window in tokens.
#         context: 128000
#         # Maximum output tokens.
#         max_tokens: 16384
#         # Whether the model supports tool calls.
#         tools: true
#         # Whether the model supports reasoning tokens.
#         reasoning: false
#         # Supported output modalities, e.g. text.
#         output:
#           - text
#         # Named provider request options.
#         variants:
#           high:
#             # Reasoning effort passed to the provider, e.g. low/medium/high.
#             reasoning_effort: high

# Model Context Protocol servers to start.
# mcp:
#   server-name:
#     # Transport: 'stdio' or 'http'.
#     transport: stdio
#     # Absolute path to the server executable for stdio.
#     command: /absolute/path/to/server
#     # Arguments for the server command.
#     args:
#       - --stdio
#     # Environment variables for the server process.
#     env:
#       NAME: value
#     # Working directory for the server process.
#     cwd: /absolute/working/directory
#     # Endpoint URL for http transport.
#     url: https://mcp.example.test/rpc
#     # Extra headers for http requests. Do not store secrets here.
#     headers:
#       X-Tenant: non-secret-value
#     # Whether the server is started.
#     enabled: false
#     # Allow plain HTTP for localhost endpoints.
#     allow_insecure_localhost: false
#     # Milliseconds to wait for the server to start; zero uses default.
#     startup_timeout_ms: 15000
#     # Milliseconds to wait for a call; zero uses default.
#     call_timeout_ms: 30000

# Language servers for the workspace.
# lsp:
#   go:
#     # Absolute path to the language server executable.
#     command: /absolute/path/to/gopls
#     # Arguments for the language server.
#     args:
#       - serve
#     # Environment variables for the language server.
#     env:
#       GOTOOLCHAIN: local
#     # File extensions associated with this server.
#     extensions:
#       - .go
#     # Maps file extensions to language IDs.
#     languages:
#       .go: go
#     # Milliseconds to wait for responses; zero uses default.
#     timeout_ms: 15000

# Ordered sandbox rules applied after base workspace mounts. Each rule
# has a path and an action: allow_write, deny_read, allow_read, or
# deny_write. Later rules override earlier ones. Requires global
# configuration.
# sandbox_rules:
#   - path: /absolute/path/to/dir
#     rule: allow_write
#   - path: /another/path
#     rule: deny_read

# Web fetch restrictions.
web_fetch:
  # Allow fetching from private addresses; increases SSRF risk.
  allow_private: false
`

// writeDefaultConfig writes a commented starter template to path.
func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigYAML), 0o600)
}

// UpdateDefaultModel updates or adds the top-level "model" field in a YAML
// config file at path, preserving comments and other fields. The value must
// be in provider/model format. It is a no-op when the file already contains
// the same value.
func UpdateDefaultModel(path, model string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config for model update: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config for model update: %w", err)
	}

	// Walk to the root mapping node.
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return errors.New("config root must be a mapping")
	}

	// Check whether the model key already exists.
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == "model" {
			if root.Content[i+1].Value == model {
				return nil // already set; nothing to do
			}
			root.Content[i+1].Value = model
			out, err := yaml.Marshal(&doc)
			if err != nil {
				return fmt.Errorf("encode updated config: %w", err)
			}
			return os.WriteFile(path, out, 0o600)
		}
	}

	// Not found; insert at the beginning of the mapping.
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "model"}
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: model}
	root.Content = append([]*yaml.Node{key, value}, root.Content...)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encode updated config: %w", err)
	}
	return os.WriteFile(path, out, 0o600)
}
