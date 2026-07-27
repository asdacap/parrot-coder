// Package config discovers, parses, and merges Parrot configuration files.
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/amirulashraf/parrot-coder/internal/atomicfile"
	"go.yaml.in/yaml/v3"
)

const (
	FileName                      = "parrot.yaml"
	PredefinedFileName            = "predefined_config.yaml"
	maxConfigBytes                = 4 << 20
	maxPermissionRequestTimeoutMS = int64((1<<63 - 1) / time.Millisecond)
)

// Config is the typed configuration consumed by later application phases.
type Config struct {
	Prompt                     string                `json:"prompt,omitempty"`
	DefaultModel               string                `json:"model,omitempty"`
	ModelAliases               map[string]ModelAlias `json:"model_aliases,omitempty"`
	ModelAugmentSystemPrompts  map[string]string     `json:"model_augment_system_prompts,omitempty"`
	InlineDiff                 bool                  `json:"inline_diff,omitempty"`
	PermissionRequestTimeoutMS int                   `json:"permission_request_timeout_ms,omitempty"`
	Providers                  map[string]Provider   `json:"providers,omitempty"`
	MCP                        map[string]MCP        `json:"mcp,omitempty"`
	WebFetch                   WebFetch              `json:"web_fetch,omitempty"`
	Subagents                  Subagents             `json:"subagents,omitempty"`
	DefaultProfile             string                `json:"default_profile,omitempty"`
	Profiles                   map[string]Profile    `json:"profiles,omitempty"`
	ToolBlacklist              map[string]bool       `json:"tool_blacklist,omitempty"`
	SandboxRules               []SandboxRule         `json:"sandbox_rules,omitempty"`
}

// ModelAlias gives a stable name to a model selector for a particular class of
// work. An empty ModelString leaves the alias available for user configuration.
// AugmentSystemPrompt distinguishes an omitted override from an explicit empty
// augmentation.
type ModelAlias struct {
	ModelString         string  `json:"model_string"`
	Usage               string  `json:"usage"`
	AugmentSystemPrompt *string `json:"augment_system_prompt,omitempty"`
}

// Subagents controls child-agent concurrency and nesting.
type Subagents struct {
	MaxConcurrent          int `json:"max_concurrent"`
	MaxConcurrentPerParent int `json:"max_concurrent_per_parent"`
	MaxDepth               int `json:"max_depth"`
}

// Profile configures one foreground mode or child agent profile.
type Profile struct {
	Prompt         string        `json:"prompt"`
	Usage          string        `json:"usage"`
	HardRules      []string      `json:"hard_rules"`
	MaxTurns       int           `json:"max_turns"`
	RecursionLimit int           `json:"recursion_limit"`
	ReadOnly       bool          `json:"read_only"`
	IsUserAgent    bool          `json:"is_user_agent"`
	AllowedTools   []string      `json:"allowed_tools,omitempty"`
	SandboxRules   []SandboxRule `json:"sandbox_rules,omitempty"`
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
	// LegacyVariant carries a former top-level variant until application startup
	// can combine it with a model restored from session history.
	LegacyVariant string
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
	defaults, err := Parse(predefinedConfigYAML)
	if err != nil {
		return Result{}, fmt.Errorf("parse predefined config: %w", err)
	}
	merged := make(map[string]any)
	provenance := make(map[string]string)
	mergeObject(merged, defaults, "", PredefinedFileName, provenance)
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
	// Obsolete configuration is ignored so existing files remain usable after
	// the corresponding features are removed.
	delete(merged, "snapshot")
	delete(merged, "lsp")
	stripLegacyProfileStatuses(merged, provenance)
	legacyVariant, err := combineLegacyVariant(merged)
	if err != nil {
		return Result{}, err
	}
	delete(provenance, "variant")

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
	if typed.ModelAliases == nil {
		typed.ModelAliases = make(map[string]ModelAlias)
	}
	if typed.ModelAugmentSystemPrompts == nil {
		typed.ModelAugmentSystemPrompts = make(map[string]string)
	}
	if typed.ToolBlacklist == nil {
		typed.ToolBlacklist = make(map[string]bool)
	}
	if typed.Profiles == nil {
		typed.Profiles = make(map[string]Profile)
	}
	if err := validateModelAliases(typed.ModelAliases); err != nil {
		return Result{}, err
	}
	if err := validateModelAugmentSystemPrompts(typed.ModelAugmentSystemPrompts); err != nil {
		return Result{}, err
	}
	if err := validateToolBlacklist(typed.ToolBlacklist); err != nil {
		return Result{}, err
	}
	if err := validateSubagents(typed.Subagents); err != nil {
		return Result{}, err
	}
	if err := validateProfiles(typed.DefaultProfile, typed.Profiles); err != nil {
		return Result{}, err
	}
	if typed.PermissionRequestTimeoutMS <= 0 {
		return Result{}, errors.New("permission_request_timeout_ms must be greater than zero")
	}
	if int64(typed.PermissionRequestTimeoutMS) > maxPermissionRequestTimeoutMS {
		return Result{}, fmt.Errorf("permission_request_timeout_ms must not exceed %d", maxPermissionRequestTimeoutMS)
	}
	if options.ConfigDir != "" {
		if err := WritePredefinedConfig(filepath.Join(options.ConfigDir, PredefinedFileName)); err != nil {
			return Result{}, fmt.Errorf("write predefined config: %w", err)
		}
	}
	return Result{Config: typed, Sources: sources, Provenance: provenance, LegacyVariant: legacyVariant}, nil
}

// stripLegacyProfileStatuses removes the retired profile status field at the
// untyped compatibility boundary. This lets existing configuration continue to
// load while strict decoding still rejects all other unknown fields.
func stripLegacyProfileStatuses(merged map[string]any, provenance map[string]string) {
	profiles, ok := merged["profiles"].(map[string]any)
	if !ok {
		return
	}
	for id, value := range profiles {
		profile, ok := value.(map[string]any)
		if !ok {
			continue
		}
		delete(profile, "status")
		prefix := "profiles." + id + ".status"
		for path := range provenance {
			if path == prefix || strings.HasPrefix(path, prefix+".") {
				delete(provenance, path)
			}
		}
	}
}

// combineLegacyVariant migrates the former top-level variant field into the
// canonical provider/model[/variant] selector before strict typed decoding.
// Keeping the compatibility handling in the untyped boundary prevents legacy
// state from leaking into the active Config model.
func combineLegacyVariant(merged map[string]any) (string, error) {
	value, exists := merged["variant"]
	if !exists {
		return "", nil
	}
	delete(merged, "variant")
	variant, ok := value.(string)
	if !ok {
		return "", errors.New("variant must be a string")
	}
	if variant == "" {
		return "", nil
	}
	model, ok := merged["model"].(string)
	if !ok || model == "" {
		return variant, nil
	}
	if selectorHasConfiguredVariant(model, merged["providers"]) {
		return "", fmt.Errorf("model selection %q already encodes a variant; remove the legacy variant field", model)
	}
	merged["model"] = model + "/" + variant
	return "", nil
}

// selectorHasConfiguredVariant recognizes conflicts without assuming model IDs
// cannot contain slashes. A final segment is a variant only when the preceding
// model exposes that name; an exact model with the same selector remains an
// ambiguity and is therefore also rejected.
func selectorHasConfiguredVariant(selector string, providersValue any) bool {
	providerID, remainder, found := strings.Cut(selector, "/")
	if !found || providerID == "" || remainder == "" {
		return false
	}
	providers, _ := providersValue.(map[string]any)
	providerConfig, _ := providers[providerID].(map[string]any)
	models, _ := providerConfig["models"].(map[string]any)
	modelID, variant, found := strings.Cut(remainder, "/")
	for found {
		modelConfig, modelExists := models[modelID].(map[string]any)
		variants, _ := modelConfig["variants"].(map[string]any)
		_, variantExists := variants[variant]
		if modelExists && variantExists {
			return true
		}
		next, suffix, more := strings.Cut(variant, "/")
		if !more {
			break
		}
		modelID, variant, found = modelID+"/"+next, suffix, true
	}
	return false
}

func validateModelAliases(aliases map[string]ModelAlias) error {
	for name, alias := range aliases {
		prefix := "model_aliases." + name
		if name == "" {
			return errors.New("model_aliases key must not be empty")
		}
		if strings.TrimSpace(name) != name {
			return fmt.Errorf("model alias name %q must not have surrounding whitespace", name)
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("model alias name %q must not contain '/'", name)
		}
		if alias.Usage == "" {
			return fmt.Errorf("%s.usage must not be empty", prefix)
		}
		if err := validateModelSelector(prefix+".model_string", alias.ModelString, true); err != nil {
			return err
		}
	}
	return nil
}

func validateModelAugmentSystemPrompts(prompts map[string]string) error {
	for selector := range prompts {
		if err := validateModelSelector("model_augment_system_prompts key", selector, false); err != nil {
			return err
		}
	}
	return nil
}

func validateModelSelector(path, selector string, allowEmpty bool) error {
	if selector == "" && allowEmpty {
		return nil
	}
	if strings.TrimSpace(selector) != selector {
		return fmt.Errorf("%s must not have surrounding whitespace", path)
	}
	for _, character := range selector {
		if unicode.IsControl(character) && unicode.IsSpace(character) {
			return fmt.Errorf("%s must not contain control whitespace", path)
		}
	}
	segments := strings.Split(selector, "/")
	if len(segments) < 2 {
		return fmt.Errorf("%s must be provider/model, optionally followed by /variant", path)
	}
	for _, segment := range segments {
		if segment == "" {
			return fmt.Errorf("%s must not contain empty path segments", path)
		}
	}
	return nil
}

func validateToolBlacklist(blacklist map[string]bool) error {
	for id := range blacklist {
		if id == "" {
			return errors.New("tool_blacklist key must not be empty")
		}
		if strings.TrimSpace(id) != id {
			return fmt.Errorf("tool_blacklist key %q must not have surrounding whitespace", id)
		}
	}
	return nil
}

func validateProfiles(defaultProfile string, profiles map[string]Profile) error {
	if defaultProfile == "" {
		return errors.New("default_profile is required")
	}
	profile, ok := profiles[defaultProfile]
	if !ok {
		return fmt.Errorf("default_profile %q is not configured in profiles", defaultProfile)
	}
	if !profile.IsUserAgent {
		return fmt.Errorf("default_profile %q is not a user agent profile", defaultProfile)
	}
	required := map[string]bool{"build": false, "plan": false, "query": false, "explorer": false, "review": false, "worker": false, "thinker": false}
	for id, profile := range profiles {
		path := "profiles." + id
		if _, ok := required[id]; !ok {
			return fmt.Errorf("%s is not a supported profile", path)
		}
		required[id] = true
		if strings.TrimSpace(profile.Prompt) == "" {
			return fmt.Errorf("%s.prompt is required", path)
		}
		if strings.TrimSpace(profile.Usage) == "" {
			return fmt.Errorf("%s.usage is required", path)
		}
		if profile.MaxTurns <= 0 {
			return fmt.Errorf("%s.max_turns must be greater than zero", path)
		}
		if profile.RecursionLimit < 0 {
			return fmt.Errorf("%s.recursion_limit must not be negative", path)
		}
		allowedTools := make(map[string]struct{}, len(profile.AllowedTools))
		for index, id := range profile.AllowedTools {
			if id == "" {
				return fmt.Errorf("%s.allowed_tools.%d must not be empty", path, index)
			}
			if strings.TrimSpace(id) != id {
				return fmt.Errorf("%s.allowed_tools.%d must not have surrounding whitespace", path, index)
			}
			if _, exists := allowedTools[id]; exists {
				return fmt.Errorf("%s.allowed_tools.%d duplicates %q", path, index, id)
			}
			allowedTools[id] = struct{}{}
		}
		for index, rule := range profile.SandboxRules {
			if strings.TrimSpace(rule.Path) == "" {
				return fmt.Errorf("%s.sandbox_rules.%d.path is required", path, index)
			}
			switch rule.Rule {
			case "allow_write", "deny_read", "allow_read", "deny_write":
			default:
				return fmt.Errorf("%s.sandbox_rules.%d.rule is invalid", path, index)
			}
		}
	}
	for id, configured := range required {
		if !configured {
			return fmt.Errorf("profiles.%s is required", id)
		}
	}
	return nil
}

func validateSubagents(value Subagents) error {
	if value.MaxConcurrent <= 0 {
		return errors.New("subagents.max_concurrent must be greater than zero")
	}
	if value.MaxConcurrentPerParent <= 0 {
		return errors.New("subagents.max_concurrent_per_parent must be greater than zero")
	}
	if value.MaxConcurrentPerParent > value.MaxConcurrent {
		return errors.New("subagents.max_concurrent_per_parent must not exceed subagents.max_concurrent")
	}
	if value.MaxDepth <= 0 {
		return errors.New("subagents.max_depth must be greater than zero")
	}
	return nil
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
	switch value := value.(type) {
	case map[string]any:
		if len(value) == 0 {
			provenance[path] = source
			return
		}
		for key, child := range value {
			markProvenance(child, path+"."+key, source, provenance)
		}
	case []any:
		if len(value) == 0 {
			provenance[path] = source
			return
		}
		for index, child := range value {
			markProvenance(child, fmt.Sprintf("%s.%d", path, index), source, provenance)
		}
	default:
		provenance[path] = source
	}
}

// predefinedConfigYAML is the embedded, always-overwritten configuration
// reference. It documents every default field and is the source of runtime
// defaults.
//
//go:embed predefined_config.yaml
var predefinedConfigYAML []byte

// WritePredefinedConfig writes the predefined configuration reference YAML to
// the specified path. The file is always overwritten; it documents every default
// configuration field for the agent to read programmatically.
func WritePredefinedConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, predefinedConfigYAML, 0o600)
}

// defaultConfigYAML is a commented starter template. Example values are
// commented out so the file is safe to parse; only web_fetch carries the
// safe default value so the document root remains a valid YAML object.
const defaultConfigYAML = `# Parrot Coder configuration file.
# Uncomment and edit the sections below to configure Parrot.

# Base agent prompt included in the system context. An override replaces the
# complete predefined prompt rather than appending to it.
# prompt: |-
#   You are Parrot Coder, a local coding agent.

# Default model selected as provider/model, optionally followed by /variant.
# model: provider/model/high

# Stable aliases for model selectors. Alias entries and their fields merge by
# key across configuration layers, so partial overrides inherit other fields.
# model_aliases:
#   low_llm:
#     model_string: provider/model/low
#     usage: Fast, inexpensive tasks
#     augment_system_prompt: Prefer concise answers.

# Extra system prompts keyed by an exact provider/model[/variant] selector.
# Set a value to "" to explicitly clear an inherited augmentation.
# model_augment_system_prompts:
#   provider/model/low: Prefer concise answers.

# Render changed lines inline. Set to false for a side-by-side diff viewer.
# inline_diff: true

# Positive number of milliseconds to wait for a permission request response.
# permission_request_timeout_ms: 30000

# Default foreground profile. Profile defaults and definitions are in predefined_config.yaml.
# default_profile: build

# Override individual built-in profile fields; unspecified fields inherit their defaults.
# profiles:
#   worker:
#     max_turns: 96
#   explorer:
#     recursion_limit: 2

# Tool blacklist keyed by tool ID. True disables a tool; false in a
# higher-precedence layer re-enables an inherited blacklist entry.
# tool_blacklist:
#   web_fetch: true

# Child-agent concurrency and nesting limits. Defaults are defined in predefined_config.yaml.
# subagents:
#   # Maximum child-agent turns running across the process.
#   max_concurrent: 64
#   # Maximum child-agent turns running for one parent session.
#   max_concurrent_per_parent: 16
#   # Maximum number of nested child-agent levels.
#   max_depth: 4

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

# Ordered sandbox rules applied after the built-in mounts below.
# Each rule has a path and an action: allow_write, deny_read,
# allow_read, or deny_write. Later rules override earlier ones.
# Requires global configuration.
#
# Built-in rules (applied first, in order):
#   /                             allow_read    (entire host filesystem)
#   /dev                          sandbox tmpfs (empty /dev)
#   /dev/null                     allow_write   (writable null)
#   /dev/zero,random,urandom,...  allow_read    (host devices)
#   /proc                         sandbox procfs
#   {session temp dir} -> /tmp    allow_write   (private writable)
#   {workspace root}              allow_write   (always writable)
#   {session grants}              allow_write   (from request_write_permission)
#   {linked git common dir}       allow_write   (worktree support)
#   {workspace}/.parrot           deny_write    (protected metadata)
#   {workspace}/parrot.yaml       deny_write    (protected metadata)
#
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

// UpdateModelAlias updates one alias model selector while preserving comments,
// unrelated fields, and inherited alias configuration. If path does not exist,
// it is created.
func UpdateModelAlias(path, name, model string) error {
	if err := validateModelAliases(map[string]ModelAlias{name: {ModelString: model, Usage: "configured"}}); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config for model alias update: %w", err)
	}

	var doc yaml.Node
	if errors.Is(err, os.ErrNotExist) || len(bytes.TrimSpace(data)) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	} else if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config for model alias update: %w", err)
	}
	if err := rejectDuplicateYAMLKeys(&doc, "$"); err != nil {
		return fmt.Errorf("parse config for model alias update: %w", err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return errors.New("config root must be a mapping")
	}

	aliases := mappingValue(root, "model_aliases")
	if aliases == nil {
		aliases = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setTopLevelNode(root, "model_aliases", aliases)
	} else if aliases.Kind != yaml.MappingNode {
		return errors.New("model_aliases must be a mapping")
	}
	alias := mappingValue(aliases, name)
	if alias == nil {
		alias = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		aliases.Content = append(aliases.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, alias)
	} else if alias.Kind != yaml.MappingNode {
		return fmt.Errorf("model_aliases.%s must be a mapping", name)
	}
	if !setMappingScalar(alias, "model_string", model) {
		return nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encode updated config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory for model alias update: %w", err)
	}
	if err := atomicfile.Write(path, out); err != nil {
		return fmt.Errorf("write model alias update: %w", err)
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setMappingScalar(node *yaml.Node, key, value string) bool {
	if existing := mappingValue(node, key); existing != nil {
		if existing.Kind == yaml.ScalarNode && existing.Value == value {
			return false
		}
		existing.Kind, existing.Tag, existing.Value, existing.Content = yaml.ScalarNode, "!!str", value, nil
		return true
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
	return true
}

func setTopLevelNode(root *yaml.Node, key string, value *yaml.Node) bool {
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value != key {
			continue
		}
		existing := root.Content[i+1]
		encodedExisting, _ := yaml.Marshal(existing)
		encodedValue, _ := yaml.Marshal(value)
		if bytes.Equal(encodedExisting, encodedValue) {
			return false
		}
		value.HeadComment = existing.HeadComment
		value.LineComment = existing.LineComment
		value.FootComment = existing.FootComment
		root.Content[i+1] = value
		return true
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
	return true
}

// UpdateDefaultSelection updates the canonical top-level "model" field,
// preserving comments and unrelated fields. The obsolete top-level "variant"
// field is removed. If path does not exist, it is created.
func UpdateDefaultSelection(path, model string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config for selection update: %w", err)
	}

	var doc yaml.Node
	if errors.Is(err, os.ErrNotExist) || len(bytes.TrimSpace(data)) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	} else if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config for selection update: %w", err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return errors.New("config root must be a mapping")
	}

	changed := removeTopLevelField(root, "variant")
	changed = setTopLevelScalar(root, "model", model) || changed
	if !changed {
		return nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encode updated config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory for selection update: %w", err)
	}
	if err := atomicfile.Write(path, out); err != nil {
		return fmt.Errorf("write config selection update: %w", err)
	}
	return nil
}

func setTopLevelScalar(root *yaml.Node, key, value string) bool {
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == key {
			if root.Content[i+1].Value == value {
				return false
			}
			node := root.Content[i+1]
			node.Kind, node.Tag, node.Value, node.Content = yaml.ScalarNode, "!!str", value, nil
			return true
		}
	}
	root.Content = append([]*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	}, root.Content...)
	return true
}

func removeTopLevelField(root *yaml.Node, key string) bool {
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == key {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			return true
		}
	}
	return false
}
