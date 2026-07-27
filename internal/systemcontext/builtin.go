package systemcontext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type StaticSource struct {
	SourceKey string
	Text      string
}

func (s StaticSource) Key() string { return s.SourceKey }
func (s StaticSource) Observe(context.Context) (Observation, error) {
	return Observation{Available: true, Text: s.Text}, nil
}

type DateSource struct{ Now func() time.Time }

func (DateSource) Key() string { return "runtime:date" }
func (s DateSource) Observe(context.Context) (Observation, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	return Observation{Available: true, Text: "Current date: " + now.Format("2006-01-02")}, nil
}

type EnvironmentSource struct {
	WorkingDirectory     string
	ProjectRoot          string
	ProjectID            string
	ConfigPath           string
	PredefinedConfigPath string
}

func (EnvironmentSource) Key() string { return "runtime:environment" }
func (s EnvironmentSource) Observe(context.Context) (Observation, error) {
	value := struct {
		OS, Arch, WorkingDirectory, ProjectRoot, ProjectID, ConfigPath, PredefinedConfigPath string
	}{runtime.GOOS, runtime.GOARCH, s.WorkingDirectory, s.ProjectRoot, s.ProjectID, s.ConfigPath, s.PredefinedConfigPath}
	text := fmt.Sprintf("Platform: %s/%s\nWorking directory: %s\nProject root: %s", value.OS, value.Arch, value.WorkingDirectory, value.ProjectRoot)
	if value.ProjectID != "" {
		text += "\nProject ID: " + value.ProjectID
	}
	if value.ConfigPath != "" {
		text += "\nConfig file: " + value.ConfigPath
	}
	if value.PredefinedConfigPath != "" {
		text += "\nPredefined config path: " + value.PredefinedConfigPath
	}
	return Observation{Available: true, Text: text}, nil
}

type CLIUtilitiesSource struct {
	Available []string
}

func (CLIUtilitiesSource) Key() string { return "runtime:cli-utilities" }
func (s CLIUtilitiesSource) Observe(context.Context) (Observation, error) {
	utilities := "none"
	if len(s.Available) > 0 {
		utilities = strings.Join(s.Available, ", ")
	}
	return Observation{Available: true, Text: "Available CLI utilities: " + utilities}, nil
}

type OptionalCLIUtilitiesSource struct {
	Available []string
}

func (OptionalCLIUtilitiesSource) Key() string { return "runtime:optional-cli-utilities" }
func (s OptionalCLIUtilitiesSource) Observe(context.Context) (Observation, error) {
	utilities := "none"
	if len(s.Available) > 0 {
		utilities = strings.Join(s.Available, ", ")
	}
	return Observation{Available: true, Text: "Available optional CLI utilities: " + utilities}, nil
}

// ModelAlias describes one usable short name without coupling system context
// construction to the configuration package.
type ModelAlias struct {
	Name        string
	ModelString string
	Usage       string
}

type ModelAliasesSource struct {
	mu      sync.RWMutex
	aliases []ModelAlias
}

func NewModelAliasesSource(aliases []ModelAlias) *ModelAliasesSource {
	s := &ModelAliasesSource{}
	s.Set(aliases)
	return s
}

func (*ModelAliasesSource) Key() string { return "runtime:model-aliases" }

// Set atomically replaces the aliases observed by future system-context builds.
func (s *ModelAliasesSource) Set(aliases []ModelAlias) {
	s.mu.Lock()
	s.aliases = append([]ModelAlias(nil), aliases...)
	s.mu.Unlock()
}

func (s *ModelAliasesSource) Observe(context.Context) (Observation, error) {
	s.mu.RLock()
	configured := append([]ModelAlias(nil), s.aliases...)
	s.mu.RUnlock()
	aliases := make([]ModelAlias, 0, len(configured))
	for _, alias := range configured {
		if alias.ModelString != "" {
			aliases = append(aliases, alias)
		}
	}
	if len(aliases) == 0 {
		return Observation{Available: true}, nil
	}
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Name < aliases[j].Name })
	var text strings.Builder
	text.WriteString("Configured model aliases may be passed anywhere a model selector is accepted, especially the agent_spawn model argument:\n")
	for _, alias := range aliases {
		fmt.Fprintf(&text, "- %s: %s — %s\n", alias.Name, alias.ModelString, alias.Usage)
	}
	return Observation{Available: true, Text: strings.TrimSuffix(text.String(), "\n")}, nil
}

// Subagent describes a profile available for delegated work.
type Subagent struct {
	ID    string
	Usage string
}

type SubagentsSource struct {
	Available []Subagent
}

func (SubagentsSource) Key() string { return "runtime:subagents" }

func (s SubagentsSource) Observe(context.Context) (Observation, error) {
	if len(s.Available) == 0 {
		return Observation{Available: true, Text: "Available subagents: none"}, nil
	}
	var text strings.Builder
	text.WriteString("Available subagents; delegate according to their configured usage:\n")
	for _, agent := range s.Available {
		fmt.Fprintf(&text, "- %s: %s\n", agent.ID, agent.Usage)
	}
	return Observation{Available: true, Text: strings.TrimSuffix(text.String(), "\n")}, nil
}

type FileSource struct {
	SourceKey string
	Path      string
	Label     string
	MaxBytes  int64
}

func (s FileSource) Key() string { return s.SourceKey }
func (s FileSource) Observe(context.Context) (Observation, error) {
	max := s.MaxBytes
	if max <= 0 {
		max = 256 << 10
	}
	file, err := os.Open(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Observation{Available: true, Path: s.Path}, nil
	}
	if err != nil {
		return Observation{Available: false}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, max+1))
	closeErr := file.Close()
	if readErr != nil {
		return Observation{Available: false}, readErr
	}
	if closeErr != nil {
		return Observation{Available: false}, closeErr
	}
	if int64(len(data)) > max {
		return Observation{Available: false}, errors.New("systemcontext: file exceeds byte limit")
	}
	return Observation{Available: true, Text: s.Label + ":\n" + string(data), Path: s.Path}, nil
}

type BuiltinOptions struct {
	AgentPrompt                   string
	Skills                        string
	ConfigDir                     string
	ConfigPath                    string
	PredefinedConfigPath          string
	ProjectRoot                   string
	WorkingDirectory              string
	ProjectID                     string
	AvailableCLIUtilities         []string
	AvailableOptionalCLIUtilities []string
	Subagents                     []Subagent
	ModelAliases                  *ModelAliasesSource
	Now                           func() time.Time
}

func Builtins(options BuiltinOptions) ([]Source, error) {
	root, cwd, err := canonicalBounds(options.ProjectRoot, options.WorkingDirectory)
	if err != nil {
		return nil, err
	}
	sources := []Source{
		StaticSource{"agent:prompt", options.AgentPrompt},
		DateSource{options.Now},
		EnvironmentSource{WorkingDirectory: cwd, ProjectRoot: root, ProjectID: options.ProjectID, ConfigPath: options.ConfigPath, PredefinedConfigPath: options.PredefinedConfigPath},
		CLIUtilitiesSource{Available: options.AvailableCLIUtilities},
		OptionalCLIUtilitiesSource{Available: options.AvailableOptionalCLIUtilities},
	}
	if options.Skills != "" {
		sources = append(sources, StaticSource{"runtime:skills", options.Skills})
	}
	if len(options.Subagents) > 0 {
		sources = append(sources, SubagentsSource{Available: options.Subagents})
	}
	if options.ModelAliases != nil {
		sources = append(sources, options.ModelAliases)
	}
	paths := []struct{ path, key, label string }{}
	if options.ConfigDir != "" {
		path := filepath.Join(options.ConfigDir, "AGENTS.md")
		paths = append(paths, struct{ path, key, label string }{path, "agents:global", "Global AGENTS.md"})
	}
	rel, _ := filepath.Rel(root, cwd)
	dirs := []string{root}
	if rel != "." {
		dir := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			dir = filepath.Join(dir, part)
			dirs = append(dirs, dir)
		}
	}
	for i, dir := range dirs {
		path := filepath.Join(dir, "AGENTS.md")
		paths = append(paths, struct{ path, key, label string }{path, fmt.Sprintf("agents:project-%04d", i), "AGENTS.md at " + dir})
	}
	for _, item := range paths {
		sources = append(sources, FileSource{SourceKey: item.key, Path: item.path, Label: item.label})
	}
	return sources, nil
}

// ObserveAgentsFiles reads the AGENTS.md sources in their effective order and
// returns the files whose contents will be included in agent context. Missing
// files are valid inputs and are omitted from the result.
func ObserveAgentsFiles(ctx context.Context, sources []Source) ([]string, error) {
	paths := make([]string, 0)
	seen := make(map[string]bool)
	var failures []error
	for _, source := range sources {
		if source == nil || !strings.HasPrefix(source.Key(), "agents:") {
			continue
		}
		observation, err := source.Observe(ctx)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", source.Key(), err))
			continue
		}
		if !observation.Available || strings.TrimSpace(observation.Text) == "" || observation.Path == "" || seen[observation.Path] {
			continue
		}
		seen[observation.Path] = true
		paths = append(paths, observation.Path)
	}
	return paths, errors.Join(failures...)
}

func canonicalBounds(root, cwd string) (string, string, error) {
	if root == "" || cwd == "" {
		return "", "", errors.New("systemcontext: project root and working directory are required")
	}
	canonical := func(path string) (string, error) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(absolute)
	}
	root, err := canonical(root)
	if err != nil {
		return "", "", err
	}
	cwd, err = canonical(cwd)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", errors.New("systemcontext: working directory is outside project root")
	}
	return filepath.Clean(root), filepath.Clean(cwd), nil
}
