// Package lsp manages Language Server Protocol processes over stdio.
package lsp

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config describes one language server. Command and Workspace must be absolute.
type Config struct {
	Name                  string
	Command               string
	Args                  []string
	Workspace             string
	Environment           map[string]string
	InitializationOptions any
	Timeout               time.Duration
	ShutdownTimeout       time.Duration
	MaxMessageBytes       int64
	MaxDiagnostics        int
	MaxDiagnosticURIs     int
}

func (c Config) normalized() (Config, error) {
	if c.Name == "" {
		c.Name = filepath.Base(c.Command)
	}
	if c.Command == "" || !filepath.IsAbs(c.Command) {
		return Config{}, errors.New("lsp: command must be absolute")
	}
	if c.Workspace == "" || !filepath.IsAbs(c.Workspace) {
		return Config{}, errors.New("lsp: workspace must be absolute")
	}
	root, err := filepath.EvalSymlinks(c.Workspace)
	if err != nil {
		return Config{}, errors.New("lsp: workspace does not exist")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return Config{}, errors.New("lsp: workspace is not a directory")
	}
	c.Workspace, err = filepath.Abs(root)
	if err != nil {
		return Config{}, err
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = time.Second
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 8 << 20
	}
	if c.MaxDiagnostics <= 0 {
		c.MaxDiagnostics = 1_000
	}
	if c.MaxDiagnosticURIs <= 0 {
		c.MaxDiagnosticURIs = 256
	}
	for name, value := range c.Environment {
		if !validEnv(name, value) {
			return Config{}, errors.New("lsp: invalid environment")
		}
	}
	return c, nil
}

func controlledEnvironment(extra map[string]string) []string {
	values := make(map[string]string)
	for _, name := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TERM", "TMPDIR", "TZ"} {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range extra {
		if validEnv(name, value) && !unsafeEnv(name) {
			values[name] = value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env
}

func validEnv(name, value string) bool {
	return name != "" && !strings.ContainsAny(name, "=\x00") && !strings.ContainsRune(value, 0)
}

func unsafeEnv(name string) bool {
	name = strings.ToUpper(name)
	return strings.HasPrefix(name, "DYLD_") || name == "LD_PRELOAD" || name == "LD_LIBRARY_PATH" ||
		name == "BASH_ENV" || name == "ENV" || name == "ZDOTDIR" || name == "PYTHONSTARTUP" ||
		name == "PYTHONPATH" || name == "PERL5OPT" || name == "RUBYOPT" || name == "NODE_OPTIONS"
}
