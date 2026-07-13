package systemcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type StaticSource struct {
	SourceKey string
	Text      string
}

func (s StaticSource) Key() string { return s.SourceKey }
func (s StaticSource) Observe(context.Context) (Observation, error) {
	raw, _ := json.Marshal(s.Text)
	return Observation{Available: true, Value: raw, Baseline: s.Text, Update: s.Text}, nil
}

type DateSource struct{ Now func() time.Time }

func (DateSource) Key() string { return "runtime:date" }
func (s DateSource) Observe(context.Context) (Observation, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	date := now.Format("2006-01-02")
	raw, _ := json.Marshal(date)
	text := "Current date: " + date
	return Observation{Available: true, Value: raw, Baseline: text, Update: "The current date is now " + date + "."}, nil
}

type EnvironmentSource struct {
	WorkingDirectory string
	ProjectRoot      string
	ProjectID        string
}

func (EnvironmentSource) Key() string { return "runtime:environment" }
func (s EnvironmentSource) Observe(context.Context) (Observation, error) {
	value := struct {
		OS, Arch, WorkingDirectory, ProjectRoot, ProjectID string
	}{runtime.GOOS, runtime.GOARCH, s.WorkingDirectory, s.ProjectRoot, s.ProjectID}
	raw, _ := json.Marshal(value)
	text := fmt.Sprintf("Platform: %s/%s\nWorking directory: %s\nProject root: %s", value.OS, value.Arch, value.WorkingDirectory, value.ProjectRoot)
	if value.ProjectID != "" {
		text += "\nProject ID: " + value.ProjectID
	}
	return Observation{Available: true, Value: raw, Baseline: text, Update: "Environment changed:\n" + text}, nil
}

type FileSource struct {
	SourceKey string
	Path      string
	Label     string
}

func (s FileSource) Key() string { return s.SourceKey }
func (s FileSource) Observe(context.Context) (Observation, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Observation{Available: true, Removal: s.Label + " was removed."}, nil
	}
	if err != nil {
		return Observation{Available: false}, err
	}
	raw, _ := json.Marshal(string(data))
	text := s.Label + ":\n" + string(data)
	return Observation{Available: true, Value: raw, Baseline: text, Update: s.Label + " changed:\n" + string(data), Removal: s.Label + " was removed."}, nil
}

type BuiltinOptions struct {
	AgentPrompt      string
	ToolGuidance     string
	Skills           string
	ConfigDir        string
	ProjectRoot      string
	WorkingDirectory string
	ProjectID        string
	Now              func() time.Time
}

func Builtins(options BuiltinOptions) ([]Source, error) {
	root, cwd, err := canonicalBounds(options.ProjectRoot, options.WorkingDirectory)
	if err != nil {
		return nil, err
	}
	sources := []Source{
		StaticSource{"agent:prompt", options.AgentPrompt},
		DateSource{options.Now},
		EnvironmentSource{cwd, root, options.ProjectID},
	}
	if options.Skills != "" {
		sources = append(sources, StaticSource{"runtime:skills", options.Skills})
	}
	if options.ToolGuidance != "" {
		sources = append(sources, StaticSource{"runtime:tool-guidance", options.ToolGuidance})
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
		sources = append(sources, FileSource{item.key, item.path, item.label})
	}
	return sources, nil
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
