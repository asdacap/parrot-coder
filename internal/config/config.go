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

	"github.com/tailscale/hujson"
)

const (
	FileName       = "parrot.jsonc"
	maxConfigBytes = 4 << 20
)

// Config is the typed configuration consumed by later application phases.
type Config struct {
	DefaultModel string               `json:"model,omitempty"`
	Providers    map[string]Provider  `json:"providers,omitempty"`
	MCP          map[string]MCP       `json:"mcp,omitempty"`
	LSP          map[string]LSP       `json:"lsp,omitempty"`
	Formatters   map[string]Formatter `json:"formatters,omitempty"`
	WebFetch     WebFetch             `json:"web_fetch,omitempty"`
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

type Formatter struct {
	Extensions []string `json:"extensions"`
	Command    []string `json:"command"`
	Mode       string   `json:"mode,omitempty"`
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
	Models                 map[string]Model  `json:"models,omitempty"`
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
// Within one directory, .parrot/parrot.jsonc has higher precedence.
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

// Load discovers and merges all applicable files.
func Load(options Options) (Result, error) {
	sources, err := Discover(options)
	if err != nil {
		return Result{}, err
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
	if typed.Formatters == nil {
		typed.Formatters = make(map[string]Formatter)
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

// Parse converts one JSONC object into generic JSON values and rejects
// duplicate object keys at every depth.
func Parse(data []byte) (map[string]any, error) {
	value, err := hujson.Parse(data)
	if err != nil {
		return nil, err
	}
	value.Standardize()
	standard := value.Pack()
	if err := rejectDuplicateKeys(standard); err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(standard, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("config root must be a JSON object")
	}
	return object, nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key := keyToken.(string)
			if seen[key] {
				return fmt.Errorf("duplicate object key %q at %s", key, path)
			}
			seen[key] = true
			if err := scanJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	_, err = decoder.Token()
	return err
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
