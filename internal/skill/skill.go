// Package skill discovers and safely loads local skill instructions.
package skill

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
)

var (
	ErrInvalid       = errors.New("skill: invalid skill")
	ErrLimit         = errors.New("skill: limit exceeded")
	ErrOutsideRoot   = errors.New("skill: path outside discovery root")
	ErrSourceChanged = errors.New("skill: source changed since discovery")
)

type Limits struct {
	MaxSkills     int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

type Options struct {
	// GlobalConfig is the application's global config directory. Skills are
	// read from its skills child. It may be empty.
	GlobalConfig string
	ProjectRoot  string
	CWD          string
	Limits       Limits
}

type Source struct {
	Path       string
	Root       string
	Kind       string
	Precedence int
	SHA256     string
}

// Metadata deliberately excludes the instruction body.
type Metadata struct {
	Name         string
	Description  string
	Agent        string
	Model        string
	AllowedTools []string
	Source       Source
	// Provenance is ordered from lowest to highest precedence and includes the
	// selected source as its final element.
	Provenance []Source
}

type Skill struct {
	Metadata
	Prompt string
}

type Registry struct {
	limits Limits
	items  map[string]Metadata
}

func defaults(l Limits) Limits {
	if l.MaxSkills <= 0 {
		l.MaxSkills = 128
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = 256 << 10
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = 4 << 20
	}
	return l
}

// Discover returns a stable registry. A malformed discovered SKILL.md fails
// the whole operation rather than silently weakening local instructions.
func Discover(options Options) (*Registry, error) {
	limits := defaults(options.Limits)
	roots, err := discoveryRoots(options.GlobalConfig, options.ProjectRoot, options.CWD)
	if err != nil {
		return nil, err
	}
	items := make(map[string]Metadata)
	seen := 0
	var total int64
	for precedence, location := range roots {
		paths, err := skillFiles(location.root, limits.MaxSkills-seen)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			seen++
			if seen > limits.MaxSkills {
				return nil, ErrLimit
			}
			data, canonical, err := secureRead(location.root, path, limits.MaxFileBytes)
			if err != nil {
				return nil, fmt.Errorf("skill: read %q: %w", path, err)
			}
			total += int64(len(data))
			if total > limits.MaxTotalBytes {
				return nil, ErrLimit
			}
			fields, _, err := parseDocument(data, true)
			if err != nil {
				return nil, fmt.Errorf("skill: parse %q: %w", path, err)
			}
			name := fields["name"]
			if !validName(name) {
				return nil, fmt.Errorf("%w: invalid name %q", ErrInvalid, name)
			}
			source := Source{Path: canonical, Root: location.root, Kind: location.kind, Precedence: precedence, SHA256: digest(data)}
			tools, err := parseList(fields["allowed-tools"])
			if err != nil {
				return nil, fmt.Errorf("skill: parse %q: %w", path, err)
			}
			metadata := Metadata{
				Name: name, Description: fields["description"], Agent: fields["agent"], Model: fields["model"],
				AllowedTools: tools, Source: source,
			}
			if previous, ok := items[name]; ok {
				if previous.Source.Precedence == precedence {
					return nil, fmt.Errorf("%w: duplicate name %q at one precedence", ErrInvalid, name)
				}
				metadata.Provenance = append(cloneSources(previous.Provenance), source)
			} else {
				metadata.Provenance = []Source{source}
			}
			items[name] = metadata
		}
	}
	return &Registry{limits: limits, items: items}, nil
}

func (r *Registry) List() []Metadata {
	if r == nil {
		return nil
	}
	out := make([]Metadata, 0, len(r.items))
	for _, item := range r.items {
		out = append(out, cloneMetadata(item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Load(name string) (Skill, error) {
	if r == nil || !validName(name) {
		return Skill{}, ErrInvalid
	}
	metadata, ok := r.items[name]
	if !ok {
		return Skill{}, os.ErrNotExist
	}
	data, canonical, err := secureRead(metadata.Source.Root, metadata.Source.Path, r.limits.MaxFileBytes)
	if err != nil {
		return Skill{}, err
	}
	if canonical != metadata.Source.Path || digest(data) != metadata.Source.SHA256 {
		return Skill{}, ErrSourceChanged
	}
	fields, body, err := parseDocument(data, true)
	if err != nil || fields["name"] != metadata.Name {
		return Skill{}, ErrSourceChanged
	}
	return Skill{Metadata: cloneMetadata(metadata), Prompt: body}, nil
}

// RenderInstruction produces deterministic text suitable for system prompt
// injection. Callers choose the skills and their ordering is normalized here.
func RenderInstruction(skills []Skill) string {
	ordered := append([]Skill(nil), skills...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	var out strings.Builder
	for _, item := range ordered {
		fmt.Fprintf(&out, "<skill name=%s source=%s>\n%s\n</skill>\n", strconv.Quote(item.Name), strconv.Quote(item.Source.Path), strings.TrimSuffix(item.Prompt, "\n"))
	}
	return out.String()
}

type rootLocation struct {
	root string
	kind string
}

func discoveryRoots(global, projectRoot, cwd string) ([]rootLocation, error) {
	var roots []rootLocation
	if global != "" {
		configRoot, err := canonicalDir(global, true)
		if err != nil {
			return nil, err
		}
		if configRoot != "" {
			root, err := canonicalDir(filepath.Join(configRoot, "skills"), true)
			if err != nil {
				return nil, err
			}
			if root != "" {
				if !contained(configRoot, root) {
					return nil, ErrOutsideRoot
				}
				roots = append(roots, rootLocation{root: root, kind: "global"})
			}
		}
	}
	if projectRoot == "" {
		if cwd == "" {
			return roots, nil
		}
		projectRoot = cwd
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
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			at = filepath.Join(at, component)
			dirs = append(dirs, at)
		}
	}
	for _, dir := range dirs {
		skills, err := canonicalDir(filepath.Join(dir, ".parrot", "skills"), true)
		if err != nil {
			return nil, err
		}
		if skills != "" {
			if !contained(dir, skills) {
				return nil, ErrOutsideRoot
			}
			roots = append(roots, rootLocation{root: skills, kind: "project"})
		}
	}
	return roots, nil
}

func skillFiles(root string, remaining int) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
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

func parseDocument(data []byte, requireName bool) (map[string]string, string, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("%w: missing frontmatter", ErrInvalid)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil, "", fmt.Errorf("%w: unterminated frontmatter", ErrInvalid)
	}
	header := text[4 : 4+end]
	body := text[4+end+5:]
	allowed := map[string]bool{"name": true, "description": true, "agent": true, "model": true, "allowed-tools": true}
	fields := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(header))
	var listKey string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "  - ") && listKey == "allowed-tools" {
			value, err := scalar(strings.TrimSpace(strings.TrimPrefix(line, "  - ")))
			if err != nil || value == "" {
				return nil, "", fmt.Errorf("%w: malformed allowed-tools", ErrInvalid)
			}
			if fields[listKey] != "" {
				fields[listKey] += ","
			}
			fields[listKey] += value
			continue
		}
		listKey = ""
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return nil, "", fmt.Errorf("%w: malformed header line", ErrInvalid)
		}
		key, raw, ok := strings.Cut(line, ":")
		if !ok || !allowed[key] {
			return nil, "", fmt.Errorf("%w: unknown or malformed key %q", ErrInvalid, key)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, "", fmt.Errorf("%w: duplicate key %q", ErrInvalid, key)
		}
		raw = strings.TrimSpace(raw)
		if key == "allowed-tools" && raw == "" {
			fields[key] = ""
			listKey = key
			continue
		}
		value, err := scalar(raw)
		if err != nil {
			return nil, "", err
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	if requireName && fields["name"] == "" || fields["description"] == "" {
		return nil, "", fmt.Errorf("%w: name and description are required", ErrInvalid)
	}
	return fields, body, nil
}

func scalar(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '[' {
		if raw[len(raw)-1] != ']' {
			return "", fmt.Errorf("%w: malformed list", ErrInvalid)
		}
		return strings.TrimSpace(raw[1 : len(raw)-1]), nil
	}
	if raw[0] == '\'' {
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", fmt.Errorf("%w: malformed quoted scalar", ErrInvalid)
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	}
	if raw[0] == '"' {
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("%w: malformed quoted scalar", ErrInvalid)
		}
		return value, nil
	}
	if strings.ContainsAny(raw, "{}#\t") {
		return "", fmt.Errorf("%w: unsupported scalar", ErrInvalid)
	}
	return strings.TrimSpace(raw), nil
}

func parseList(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool)
	for _, part := range parts {
		part, err := scalar(strings.TrimSpace(part))
		if err != nil || part == "" {
			return nil, fmt.Errorf("%w: malformed allowed-tools list", ErrInvalid)
		}
		if !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	return out, nil
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 128 {
		return false
	}
	for _, char := range name {
		if char > 127 || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func canonicalDir(path string, optional bool) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
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

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneSources(in []Source) []Source { return append([]Source(nil), in...) }

func cloneMetadata(in Metadata) Metadata {
	in.AllowedTools = append([]string(nil), in.AllowedTools...)
	in.Provenance = cloneSources(in.Provenance)
	return in
}
