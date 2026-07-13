package config

import (
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

func TestParseJSONCAndRejectDuplicateKeys(t *testing.T) {
	parsed, err := Parse([]byte(`{
		// comment
		"model": "local/code", // trailing comma
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if parsed["model"] != "local/code" {
		t.Fatalf("parsed = %#v", parsed)
	}

	for _, input := range []string{
		`{"model":"a","model":"b"}`,
		`{"providers":{"local":{"base_url":"a","base_url":"b"}}}`,
	} {
		if _, err := Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("Parse(%s) error = %v", input, err)
		}
	}
	if _, err := Parse([]byte(`[1, 2]`)); err == nil {
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
	writeFile(t, global, `{
		"model": "openai/small",
		"providers": {"openai": {
			"type": "openai-compatible",
			"protocol": "responses",
			"base_url": "https://api.example/v1",
			"api_key_env": "OPENAI_API_KEY",
			"headers": {"X-Tenant": "one"},
			"allow_insecure_localhost": true,
			"models": {"small": {"context": 1000, "tools": true, "reasoning": true, "output": ["text"]}}
		}}
	}`)
	writeFile(t, projectFile, `{
		"model": "openai/large",
		"providers": {"openai": {"models": {"large": {"context": 2000}}}}
	}`)
	writeFile(t, nestedFile, `{
		"providers": {"openai": {"models": {"large": {"max_tokens": 500}}}}
	}`)

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
	if provider.Type != "openai-compatible" || provider.Protocol != "responses" || provider.Headers["X-Tenant"] != "one" || !provider.AllowInsecureLocalhost {
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
}

func TestLoadPhase9MapsMergeAndDecode(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	writeFile(t, filepath.Join(configDir, FileName), `{
		"mcp": {"docs": {"transport":"http","url":"https://example.test/mcp","enabled":true,"headers":{"X-One":"1"},"startup_timeout_ms":1000,"call_timeout_ms":2000}},
		"lsp": {"go": {"command":"/usr/bin/false","args":["serve"],"env":{"A":"B"},"extensions":[".go"],"languages":{".go":"go"}}},
		"formatters": {"gofmt": {"extensions":["go"],"command":["/usr/bin/gofmt"],"mode":"stdin"}},
		"web_fetch": {"allow_private": true}
	}`)
	writeFile(t, filepath.Join(project, FileName), `{"mcp":{"docs":{"headers":{"X-Two":"2"}}},"lsp":{"go":{"timeout_ms":3000}}}`)
	result, err := Load(Options{ConfigDir: configDir, ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	server := result.Config.MCP["docs"]
	if server.Transport != "http" || !server.Enabled || server.StartupTimeoutMS != 1000 || server.CallTimeoutMS != 2000 || server.Headers["X-One"] != "1" || server.Headers["X-Two"] != "2" {
		t.Fatalf("MCP = %#v", server)
	}
	language := result.Config.LSP["go"]
	if language.Command != "/usr/bin/false" || language.TimeoutMS != 3000 || language.Languages[".go"] != "go" {
		t.Fatalf("LSP = %#v", language)
	}
	if formatter := result.Config.Formatters["gofmt"]; formatter.Mode != "stdin" || !reflect.DeepEqual(formatter.Extensions, []string{"go"}) {
		t.Fatalf("formatter = %#v", formatter)
	}
	if !result.Config.WebFetch.AllowPrivate {
		t.Fatal("web_fetch.allow_private was not decoded")
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
