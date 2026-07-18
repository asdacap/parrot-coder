// Package command discovers and expands local markdown commands.
package command

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var (
	ErrInvalid       = errors.New("command: invalid command")
	ErrLimit         = errors.New("command: limit exceeded")
	ErrOutsideRoot   = errors.New("command: path outside permitted root")
	ErrShell         = errors.New("command: shell substitution is forbidden")
	ErrSourceChanged = errors.New("command: source changed since discovery")
)

type Limits struct {
	MaxCommands      int
	MaxCommandBytes  int64
	MaxFileBytes     int64
	MaxFiles         int
	MaxExpandedBytes int64
	MaxTotalBytes    int64
}

type Options struct {
	GlobalConfig string
	ProjectRoot  string
	CWD          string
	Workspace    string
	Limits       Limits
}

type Source struct {
	Path       string
	Root       string
	Kind       string
	Precedence int
	SHA256     string
}

type Metadata struct {
	Name        string
	Description string
	Agent       string
	Model       string
	Subtask     bool
	Source      Source
	Provenance  []Source
}

type Command struct {
	Metadata
	Template string
}

type SourceHash struct {
	Path   string
	SHA256 string
}

type Expansion struct {
	Prompt       string
	Agent        string
	Model        string
	Subtask      bool
	SourceHashes []SourceHash
}

type Registry struct {
	workspace string
	limits    Limits
	items     map[string]Metadata
}

type Builtin struct {
	Name        string
	Description string
}

// Builtins are runtime operations, not prompt templates, and are intentionally
// excluded from discovery and expansion.
func Builtins() []Builtin {
	return []Builtin{
		{Name: "compact", Description: "compact the current conversation"},
		{Name: "new", Description: "start a new session"},
	}
}

func defaults(l Limits) Limits {
	if l.MaxCommands <= 0 {
		l.MaxCommands = 256
	}
	if l.MaxCommandBytes <= 0 {
		l.MaxCommandBytes = 256 << 10
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = 1 << 20
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = 32
	}
	if l.MaxExpandedBytes <= 0 {
		l.MaxExpandedBytes = 4 << 20
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = 8 << 20
	}
	return l
}

func Discover(options Options) (*Registry, error) {
	limits := defaults(options.Limits)
	workspace := options.Workspace
	if workspace == "" {
		workspace = options.ProjectRoot
	}
	canonicalWorkspace, err := canonicalDir(workspace, false)
	if err != nil {
		return nil, err
	}
	roots, err := discoveryRoots(options.GlobalConfig, options.ProjectRoot, options.CWD)
	if err != nil {
		return nil, err
	}
	items := make(map[string]Metadata)
	var total int64
	count := 0
	for precedence, location := range roots {
		paths, err := commandFiles(location.root, limits.MaxCommands-count)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			count++
			data, canonical, err := secureRead(location.root, path, limits.MaxCommandBytes)
			if err != nil {
				return nil, fmt.Errorf("command: read %q: %w", path, err)
			}
			total += int64(len(data))
			if total > limits.MaxTotalBytes {
				return nil, ErrLimit
			}
			fields, body, err := parseDocument(data)
			if err != nil {
				return nil, fmt.Errorf("command: parse %q: %w", path, err)
			}
			if containsShell(body) {
				return nil, fmt.Errorf("%w in %q", ErrShell, path)
			}
			relative, err := filepath.Rel(location.root, canonical)
			if err != nil {
				return nil, err
			}
			name := strings.TrimSuffix(filepath.ToSlash(relative), ".md")
			if !validName(name) {
				return nil, fmt.Errorf("%w: invalid name %q", ErrInvalid, name)
			}
			subtask, err := parseBool(fields["subtask"])
			if err != nil {
				return nil, err
			}
			source := Source{Path: canonical, Root: location.root, Kind: location.kind, Precedence: precedence, SHA256: digest(data)}
			metadata := Metadata{Name: name, Description: fields["description"], Agent: fields["agent"], Model: fields["model"], Subtask: subtask, Source: source}
			if previous, ok := items[name]; ok {
				if previous.Source.Precedence == precedence {
					return nil, fmt.Errorf("%w: duplicate name %q", ErrInvalid, name)
				}
				metadata.Provenance = append(append([]Source(nil), previous.Provenance...), source)
			} else {
				metadata.Provenance = []Source{source}
			}
			items[name] = metadata
		}
	}
	return &Registry{workspace: canonicalWorkspace, limits: limits, items: items}, nil
}

func (r *Registry) List() []Metadata {
	if r == nil {
		return nil
	}
	out := make([]Metadata, 0, len(r.items))
	for _, item := range r.items {
		item.Provenance = append([]Source(nil), item.Provenance...)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Load(name string) (Command, error) {
	if r == nil || !validName(name) {
		return Command{}, ErrInvalid
	}
	metadata, ok := r.items[name]
	if !ok {
		return Command{}, os.ErrNotExist
	}
	data, canonical, err := secureRead(metadata.Source.Root, metadata.Source.Path, r.limits.MaxCommandBytes)
	if err != nil {
		return Command{}, err
	}
	if canonical != metadata.Source.Path || digest(data) != metadata.Source.SHA256 {
		return Command{}, ErrSourceChanged
	}
	_, body, err := parseDocument(data)
	if err != nil || containsShell(body) {
		return Command{}, ErrSourceChanged
	}
	return Command{Metadata: metadata, Template: body}, nil
}

// Expand parses Arguments using a small deterministic shell-like lexer for
// positional placeholders. It never invokes a shell.
func (r *Registry) Expand(name, arguments string) (Expansion, error) {
	command, err := r.Load(name)
	if err != nil {
		return Expansion{}, err
	}
	args, err := splitArguments(arguments)
	if err != nil {
		return Expansion{}, err
	}
	prompt := substituteArguments(command.Template, arguments, args)
	if containsShell(prompt) {
		return Expansion{}, ErrShell
	}
	prompt, files, err := r.substituteFiles(prompt)
	if err != nil {
		return Expansion{}, err
	}
	hashes := []SourceHash{{Path: command.Source.Path, SHA256: command.Source.SHA256}}
	hashes = append(hashes, files...)
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].Path < hashes[j].Path })
	return Expansion{Prompt: prompt, Agent: command.Agent, Model: command.Model, Subtask: command.Subtask, SourceHashes: hashes}, nil
}

func (r *Registry) substituteFiles(template string) (string, []SourceHash, error) {
	var out strings.Builder
	var sources []SourceHash
	seen := make(map[string]bool)
	for index := 0; index < len(template); {
		if template[index] != '@' || index > 0 && !unicode.IsSpace(rune(template[index-1])) {
			out.WriteByte(template[index])
			index++
			continue
		}
		end := index + 1
		for end < len(template) && !unicode.IsSpace(rune(template[end])) {
			end++
		}
		raw := strings.TrimRight(template[index+1:end], ",.;:!?)]}")
		punctuation := template[index+1+len(raw) : end]
		if raw == "" {
			out.WriteByte('@')
			index++
			continue
		}
		path, err := resolveWorkspace(r.workspace, raw)
		if err != nil {
			return "", nil, err
		}
		data, canonical, err := secureRead(r.workspace, path, r.limits.MaxFileBytes)
		if err != nil {
			return "", nil, err
		}
		if !seen[canonical] {
			if len(sources) >= r.limits.MaxFiles {
				return "", nil, ErrLimit
			}
			seen[canonical] = true
			sources = append(sources, SourceHash{Path: canonical, SHA256: digest(data)})
		}
		out.Write(data)
		out.WriteString(punctuation)
		if int64(out.Len()) > r.limits.MaxExpandedBytes {
			return "", nil, ErrLimit
		}
		index = end
	}
	if int64(out.Len()) > r.limits.MaxExpandedBytes {
		return "", nil, ErrLimit
	}
	return out.String(), sources, nil
}

type rootLocation struct{ root, kind string }

func discoveryRoots(global, projectRoot, cwd string) ([]rootLocation, error) {
	var result []rootLocation
	if global != "" {
		configRoot, err := canonicalDir(global, true)
		if err != nil {
			return nil, err
		}
		if configRoot != "" {
			root, err := canonicalDir(filepath.Join(configRoot, "commands"), true)
			if err != nil {
				return nil, err
			}
			if root != "" {
				if !contained(configRoot, root) {
					return nil, ErrOutsideRoot
				}
				result = append(result, rootLocation{root, "global"})
			}
		}
	}
	root, err := canonicalDir(projectRoot, false)
	if err != nil {
		return nil, err
	}
	if cwd == "" {
		cwd = root
	}
	current, err := canonicalDir(cwd, false)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, current)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, ErrOutsideRoot
	}
	dirs := []string{root}
	if rel != "." {
		at := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			at = filepath.Join(at, part)
			dirs = append(dirs, at)
		}
	}
	for _, dir := range dirs {
		commands, err := canonicalDir(filepath.Join(dir, ".parrot", "commands"), true)
		if err != nil {
			return nil, err
		}
		if commands != "" {
			if !contained(dir, commands) {
				return nil, ErrOutsideRoot
			}
			result = append(result, rootLocation{commands, "project"})
		}
	}
	return result, nil
}

func commandFiles(root string, remaining int) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			paths = append(paths, path)
			if len(paths) > remaining {
				return ErrLimit
			}
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func parseDocument(data []byte) (map[string]string, string, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("%w: missing frontmatter", ErrInvalid)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil, "", fmt.Errorf("%w: unterminated frontmatter", ErrInvalid)
	}
	header, body := text[4:4+end], text[4+end+5:]
	allowed := map[string]bool{"description": true, "agent": true, "model": true, "subtask": true}
	fields := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(header))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return nil, "", fmt.Errorf("%w: malformed header", ErrInvalid)
		}
		key, raw, ok := strings.Cut(line, ":")
		if !ok || !allowed[key] {
			return nil, "", fmt.Errorf("%w: unknown key %q", ErrInvalid, key)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, "", fmt.Errorf("%w: duplicate key %q", ErrInvalid, key)
		}
		value, err := scalar(strings.TrimSpace(raw))
		if err != nil {
			return nil, "", err
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	if fields["description"] == "" {
		return nil, "", fmt.Errorf("%w: description is required", ErrInvalid)
	}
	return fields, body, nil
}

func scalar(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '"' {
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", ErrInvalid
		}
		return value, nil
	}
	if raw[0] == '\'' {
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", ErrInvalid
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	}
	if strings.ContainsAny(raw, "[]{}#\t") {
		return "", ErrInvalid
	}
	return raw, nil
}

func parseBool(value string) (bool, error) {
	if value == "" || value == "false" {
		return false, nil
	}
	if value == "true" {
		return true, nil
	}
	return false, fmt.Errorf("%w: subtask must be true or false", ErrInvalid)
}

func containsShell(text string) bool {
	for index := 0; index+1 < len(text); index++ {
		if text[index] == '!' && text[index+1] == '`' {
			return true
		}
	}
	return false
}

func splitArguments(input string) ([]string, error) {
	var result []string
	var word strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if word.Len() > 0 {
			result = append(result, word.String())
			word.Reset()
		}
	}
	for _, char := range input {
		if escaped {
			word.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				word.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if unicode.IsSpace(char) {
			flush()
			continue
		}
		word.WriteRune(char)
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("%w: unterminated argument quote", ErrInvalid)
	}
	flush()
	return result, nil
}

func substituteArguments(template, all string, args []string) string {
	var out strings.Builder
	for index := 0; index < len(template); {
		if strings.HasPrefix(template[index:], "$ARGUMENTS") {
			out.WriteString(all)
			index += len("$ARGUMENTS")
			continue
		}
		if template[index] == '$' && index+1 < len(template) {
			digit, consumed := byte(0), 0
			if template[index+1] >= '1' && template[index+1] <= '9' {
				digit, consumed = template[index+1], 2
			} else if index+3 < len(template) && template[index+1] == '{' && template[index+2] >= '1' && template[index+2] <= '9' && template[index+3] == '}' {
				digit, consumed = template[index+2], 4
			}
			if consumed != 0 {
				position := int(digit - '1')
				if position < len(args) {
					out.WriteString(args[position])
				}
				index += consumed
				continue
			}
		}
		out.WriteByte(template[index])
		index++
	}
	return out.String()
}

func validName(name string) bool {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, char := range component {
			if char > 127 || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
				return false
			}
		}
	}
	return true
}

func resolveWorkspace(root, raw string) (string, error) {
	if raw == "" || strings.IndexByte(raw, 0) >= 0 || filepath.IsAbs(raw) {
		return "", ErrOutsideRoot
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	path := filepath.Join(root, clean)
	if !contained(root, path) {
		return "", ErrOutsideRoot
	}
	return path, nil
}

func canonicalDir(path string, optional bool) (string, error) {
	if path == "" {
		return "", errors.New("command: directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if optional && errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return filepath.Clean(resolved), nil
}

func secureRead(root, path string, max int64) ([]byte, string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || !contained(root, resolved) {
		return nil, "", ErrOutsideRoot
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, max+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, "", readErr
	}
	if closeErr != nil {
		return nil, "", closeErr
	}
	if int64(len(data)) > max {
		return nil, "", ErrLimit
	}
	return data, filepath.Clean(resolved), nil
}

func contained(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
