// Package config discovers, parses, and merges Parrot configuration files.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverOrdersScopesLowToHigh(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global")
	project := filepath.Join(root, "project")
	cwd := filepath.Join(project, "one", "two")
	paths := []string{
		filepath.Join(configDir, FileName),
		filepath.Join(project, FileName),
		filepath.Join(project, ".parrot", FileName),
		filepath.Join(project, "one", FileName),
		filepath.Join(cwd, ".parrot", FileName),
	}
	for _, path := range paths {
		writeFile(t, path, "{}")
	}

	sources, err := Discover(Options{ConfigDir: configDir, ProjectRoot: project, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, source := range sources {
		got = append(got, source.Path)
	}
	if !reflect.DeepEqual(got, paths) {
		t.Fatalf("paths = %#v, want %#v", got, paths)
	}
	if sources[0].Kind != SourceGlobal || sources[1].Kind != SourceProject {
		t.Fatalf("unexpected source kinds: %#v", sources)
	}
}

func TestDiscoverRejectsCWDOutsideProject(t *testing.T) {
	root := t.TempDir()
	_, err := Discover(Options{ConfigDir: root, ProjectRoot: filepath.Join(root, "project"), CWD: filepath.Join(root, "other")})
	if err == nil {
		t.Fatal("Discover accepted cwd outside root")
	}
}

func TestParseYAMLAndRejectDuplicateKeys(t *testing.T) {
	parsed, err := Parse([]byte(`# comment
model: local/code
`))
	if err != nil {
		t.Fatal(err)
	}
	if parsed["model"] != "local/code" {
		t.Fatalf("parsed = %#v", parsed)
	}

	for _, input := range []string{
		"model: a\nmodel: b",
		"providers:\n  local:\n    base_url: a\n    base_url: b",
	} {
		if _, err := Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("Parse(%s) error = %v", input, err)
		}
	}
	if _, err := Parse([]byte("- 1\n- 2")); err == nil {
		t.Fatal("Parse accepted an array root")
	}
}

func TestLoadMergesRecursivelyAndTracksProvenance(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	nested := filepath.Join(project, "nested")
	global := filepath.Join(configDir, FileName)
	projectFile := filepath.Join(project, FileName)
	nestedFile := filepath.Join(nested, ".parrot", FileName)
	writeFile(t, global, `model: openai/small
providers:
  openai:
    type: openai-compatible
    protocol: responses
    base_url: https://api.example/v1
    api_key_env: OPENAI_API_KEY
    headers:
      X-Tenant: one
    allow_insecure_localhost: true
    header_timeout_ms: 10000
    models:
      small:
        context: 1000
        tools: true
        reasoning: true
        output:
          - text
`)
	writeFile(t, projectFile, `model: openai/large
providers:
  openai:
    models:
      large:
        context: 2000
`)
	writeFile(t, nestedFile, `providers:
  openai:
    models:
      large:
        max_tokens: 500
tool_blacklist:
  - unrestricted_shell
  - web_fetch
`)

	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: nested})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.DefaultModel != "openai/large" {
		t.Fatalf("DefaultModel = %q", result.Config.DefaultModel)
	}
	provider := result.Config.Providers["openai"]
	if provider.BaseURL != "https://api.example/v1" || provider.APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("provider = %#v", provider)
	}
	if provider.Type != "openai-compatible" || provider.Protocol != "responses" || provider.Headers["X-Tenant"] != "one" || !provider.AllowInsecureLocalhost || provider.HeaderTimeoutMS == nil || *provider.HeaderTimeoutMS != 10000 {
		t.Fatalf("typed provider fields = %#v", provider)
	}
	if model := provider.Models["small"]; !model.Tools || !model.Reasoning || !reflect.DeepEqual(model.Output, []string{"text"}) {
		t.Fatalf("model capabilities = %#v", model)
	}
	if provider.Models["small"].Context != 1000 || provider.Models["large"].Context != 2000 || provider.Models["large"].MaxTokens != 500 {
		t.Fatalf("models = %#v", provider.Models)
	}
	if got := result.Provenance["model"]; got != projectFile {
		t.Fatalf("model provenance = %q", got)
	}
	if got := result.Provenance["providers.openai.models.large.max_tokens"]; got != nestedFile {
		t.Fatalf("max_tokens provenance = %q", got)
	}
	if got := result.Config.ToolBlacklist; len(got) != 2 || got[0] != "unrestricted_shell" || got[1] != "web_fetch" {
		t.Fatalf("ToolBlacklist = %#v", got)
	}
}

func TestLoadToolIntegrationMapsMergeAndDecode(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	writeFile(t, filepath.Join(configDir, FileName), `mcp:
  docs:
    transport: http
    url: https://example.test/mcp
    enabled: true
    headers:
      X-One: "1"
    startup_timeout_ms: 1000
    call_timeout_ms: 2000
web_fetch:
  allow_private: true
`)
	writeFile(t, filepath.Join(project, FileName), `mcp:
  docs:
    headers:
      X-Two: "2"
`)
	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	server := result.Config.MCP["docs"]
	if server.Transport != "http" || !server.Enabled || server.StartupTimeoutMS != 1000 || server.CallTimeoutMS != 2000 || server.Headers["X-One"] != "1" || server.Headers["X-Two"] != "2" {
		t.Fatalf("MCP = %#v", server)
	}
	if !result.Config.WebFetch.AllowPrivate {
		t.Fatal("web_fetch.allow_private was not decoded")
	}
}

func TestLoadRejectsUnknownTypedField(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	writeFile(t, filepath.Join(configDir, FileName), `snapshot:
  root: /legacy/journal
web_fecth:
web_fetch:
  allow_private: true
`)
	if _, err := Load(Options{ConfigDir: configDir, ProjectRoot: root, CWD: root}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load error = %v", err)
	}
	writeFile(t, filepath.Join(configDir, FileName), `snapshot:
  root: /legacy/journal
lsp:
  go:
    command: /usr/bin/gopls
`)
	if _, err := Load(Options{ConfigDir: configDir, ProjectRoot: root, CWD: root}); err != nil {
		t.Fatalf("Load rejected obsolete config: %v", err)
	}
}

func TestLoadRejectsOversizedConfig(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	writeFile(t, filepath.Join(configDir, FileName), `padding: "`+strings.Repeat("x", maxConfigBytes)+`"`)
	if _, err := Load(Options{ConfigDir: configDir, ProjectRoot: root, CWD: root}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load error = %v", err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadGeneratesDefaultConfigWhenMissing(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(result.Sources))
	}
	if result.Sources[0].Kind != SourceGlobal {
		t.Fatalf("source kind = %q, want global", result.Sources[0].Kind)
	}
	expectedPath := filepath.Join(configDir, FileName)
	if result.Sources[0].Path != expectedPath {
		t.Fatalf("source path = %q, want %q", result.Sources[0].Path, expectedPath)
	}

	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("generated YAML is not parseable: %v", err)
	}
	if !strings.Contains(string(data), "Default model selected as provider/model.") {
		t.Fatal("generated YAML missing readable comment")
	}
	if result.Config.DefaultModel != "" {
		t.Fatalf("DefaultModel = %q, want empty", result.Config.DefaultModel)
	}
	if len(result.Config.Providers) != 0 || len(result.Config.MCP) != 0 {
		t.Fatalf("generated starter should be empty, got %#v", result.Config)
	}
	if result.Config.WebFetch.AllowPrivate {
		t.Fatal("WebFetch.AllowPrivate should be false in generated starter")
	}
}

func TestLoadDoesNotOverwriteExistingConfig(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	globalPath := filepath.Join(configDir, FileName)
	writeFile(t, globalPath, "model: existing/model")

	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.DefaultModel != "existing/model" {
		t.Fatalf("DefaultModel = %q", result.Config.DefaultModel)
	}

	data, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Default model selected as provider/model.") {
		t.Fatal("existing config was overwritten with default comments")
	}
}

func TestLoadWithoutConfigDirDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	result, err := Load(Options{ConfigDir: "", ProjectRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 0 {
		t.Fatalf("sources = %d, want 0", len(result.Sources))
	}
}

func TestGeneratedYAMLHasAllReadableComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := writeDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, comment := range []string{
		"Parrot Coder configuration file.",
		"Default model selected as provider/model.",
		"OpenAI-compatible providers and their model catalogs.",
		"Provider adapter type: 'compatible' or 'openai-compatible'.",
		"API protocol: 'responses' or 'chat-completions'.",
		"Transport: 'stdio' or 'http'.",
		"Web fetch restrictions.",
		"Allow fetching from private addresses; increases SSRF risk.",
		"OpenRouter-style provider routing preferences, forwarded as the",
	} {
		if !strings.Contains(output, comment) {
			t.Errorf("generated YAML missing comment: %q", comment)
		}
	}
}

func TestLoadParsesProviderPreferences(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, FileName), `providers:
  openrouter:
    base_url: https://openrouter.ai/api/v1
    provider_preferences:
      order:
        - anthropic
      allow_fallbacks: false
`)
	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	provider := result.Config.Providers["openrouter"]
	if len(provider.ProviderPreferences) == 0 {
		t.Fatalf("provider preferences not parsed: %#v", provider)
	}
	var decoded map[string]any
	if err := json.Unmarshal(provider.ProviderPreferences, &decoded); err != nil {
		t.Fatalf("preferences are not valid JSON: %v", err)
	}
	if decoded["allow_fallbacks"] != false {
		t.Fatalf("allow_fallbacks = %#v", decoded["allow_fallbacks"])
	}
	order, _ := decoded["order"].([]any)
	if len(order) != 1 || order[0] != "anthropic" {
		t.Fatalf("order = %#v", order)
	}
}

func TestLoadSandboxRules(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(configDir, FileName), `sandbox_rules:
  - path: /opt/cache
    rule: allow_write
  - path: /secret
    rule: deny_read
  - path: /readonly/data
    rule: allow_read
  - path: /var/log
    rule: deny_write
`)
	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	rules := result.Config.SandboxRules
	if len(rules) != 4 {
		t.Fatalf("SandboxRules = %#v, want 4 rules", rules)
	}
	cases := []struct {
		path, rule string
	}{
		{"/opt/cache", "allow_write"},
		{"/secret", "deny_read"},
		{"/readonly/data", "allow_read"},
		{"/var/log", "deny_write"},
	}
	for i, want := range cases {
		if rules[i].Path != want.path || rules[i].Rule != want.rule {
			t.Fatalf("rule[%d] = {%s, %s}, want {%s, %s}", i, rules[i].Path, rules[i].Rule, want.path, want.rule)
		}
	}
}
