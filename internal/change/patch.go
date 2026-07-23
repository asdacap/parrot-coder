package change

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type PatchOperationKind string

const (
	PatchAdd    PatchOperationKind = "add"
	PatchUpdate PatchOperationKind = "update"
	PatchDelete PatchOperationKind = "delete"
)

// PatchFormat selects the edit syntax a patch is written in. Both formats parse
// into the same Patch value and share the whole apply path.
type PatchFormat string

const (
	PatchFormatAider   PatchFormat = "aider"
	PatchFormatUnified PatchFormat = "unified"
)

type PatchLine struct {
	Kind byte
	Text string
}

type PatchHunk struct {
	Lines     []PatchLine
	Context   string
	EndOfFile bool
}

type PatchOperation struct {
	Kind  PatchOperationKind
	Path  string
	Data  string
	Hunks []PatchHunk
}

type Patch struct{ Operations []PatchOperation }

// Aider format block markers.
const (
	patchSearchMarker  = "<<<<<<< SEARCH"
	patchReplaceMarker = ">>>>>>> REPLACE"
)

// ParsePatch parses the aider SEARCH/REPLACE edit format: a file path line
// followed by one or more <<<<<<< SEARCH / ======= / >>>>>>> REPLACE blocks.
// Blocks apply to the most recent path line, and an empty SEARCH section
// creates the file.
func ParsePatch(text string) (Patch, error) {
	lines, err := patchLines(text)
	if err != nil {
		return Patch{}, err
	}
	type block struct {
		path    string
		line    int
		search  []string
		replace []string
	}
	var blocks []block
	path := ""
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "```") {
			i++
			continue
		}
		if line != patchSearchMarker {
			path = line
			if err := validPatchPath(path); err != nil {
				return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
			}
			i++
			continue
		}
		if path == "" {
			return Patch{}, fmt.Errorf("%w at line %d: block has no file path", ErrInvalidPatch, i+1)
		}
		current := block{path: path, line: i + 1}
		i++
		start := i
		for i < len(lines) && !isPatchDivider(lines[i]) {
			i++
		}
		if i >= len(lines) {
			return Patch{}, fmt.Errorf("%w at line %d: SEARCH section is missing =======", ErrInvalidPatch, current.line)
		}
		current.search = lines[start:i]
		i++
		start = i
		for i < len(lines) && strings.TrimSpace(lines[i]) != patchReplaceMarker {
			i++
		}
		if i >= len(lines) {
			return Patch{}, fmt.Errorf("%w at line %d: section is missing %s", ErrInvalidPatch, current.line, patchReplaceMarker)
		}
		current.replace = lines[start:i]
		i++
		blocks = append(blocks, current)
	}
	if len(blocks) == 0 {
		return Patch{}, fmt.Errorf("%w: patch has no SEARCH/REPLACE blocks", ErrInvalidPatch)
	}
	// Merge blocks into one operation per file, keeping first-appearance
	// order so sequential edits to the same file apply in patch order.
	var patch Patch
	operations := make(map[string]int)
	for _, current := range blocks {
		index, ok := operations[current.path]
		if !ok {
			patch.Operations = append(patch.Operations, PatchOperation{Path: current.path})
			index = len(patch.Operations) - 1
			operations[current.path] = index
		}
		op := &patch.Operations[index]
		if len(current.search) == 0 {
			if op.Kind != "" {
				return Patch{}, fmt.Errorf("%w at line %d: %q mixes file creation and updates", ErrInvalidPatch, current.line, current.path)
			}
			op.Kind = PatchAdd
			for _, line := range current.replace {
				op.Data += line + "\n"
			}
			continue
		}
		if op.Kind == PatchAdd {
			return Patch{}, fmt.Errorf("%w at line %d: %q mixes file creation and updates", ErrInvalidPatch, current.line, current.path)
		}
		op.Kind = PatchUpdate
		var hunk PatchHunk
		for _, line := range current.search {
			hunk.Lines = append(hunk.Lines, PatchLine{Kind: '-', Text: line})
		}
		for _, line := range current.replace {
			hunk.Lines = append(hunk.Lines, PatchLine{Kind: '+', Text: line})
		}
		op.Hunks = append(op.Hunks, hunk)
	}
	return finalizePatch(patch)
}

// patchLines rejects NUL bytes and splits text into carriage-return-free lines.
func patchLines(text string) ([]string, error) {
	if strings.ContainsRune(text, 0) {
		return nil, fmt.Errorf("%w: NUL byte", ErrInvalidPatch)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines, nil
}

// finalizePatch applies the checks every format shares, so both parsers reject
// empty creations, duplicate paths and nested paths identically.
func finalizePatch(patch Patch) (Patch, error) {
	for _, op := range patch.Operations {
		if op.Kind == PatchAdd && op.Data == "" {
			return Patch{}, fmt.Errorf("%w: file creation for %q has no content", ErrInvalidPatch, op.Path)
		}
	}
	if err := validatePatchPaths(patch); err != nil {
		return Patch{}, err
	}
	return patch, nil
}

func isPatchDivider(line string) bool {
	line = strings.TrimSpace(line)
	return len(line) >= 7 && strings.Trim(line, "=") == ""
}

func validPatchPath(path string) error {
	if path == "" || filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return errors.New("path must be a clean workspace path")
	}
	return nil
}

func validatePatchPaths(patch Patch) error {
	var paths []string
	for _, op := range patch.Operations {
		paths = append(paths, filepath.Clean(op.Path))
	}
	sort.Strings(paths)
	for i, path := range paths {
		if i > 0 {
			previous := paths[i-1]
			if path == previous {
				return fmt.Errorf("%w: duplicate or cycling path %q", ErrInvalidPatch, path)
			}
			if strings.HasPrefix(path, previous+string(filepath.Separator)) {
				return fmt.Errorf("%w: overlapping paths %q and %q", ErrInvalidPatch, previous, path)
			}
		}
	}
	return nil
}

func (s *Service) PlanPatch(ctx context.Context, ws *workspace.Workspace, text string, format PatchFormat) (Plan, error) {
	var patch Patch
	var err error
	switch format {
	case PatchFormatAider:
		patch, err = ParsePatch(text)
	case PatchFormatUnified:
		patch, err = ParseUnifiedDiff(text)
	default:
		return Plan{}, fmt.Errorf("%w: unknown format %q", ErrInvalidPatch, format)
	}
	if err != nil {
		return Plan{}, err
	}
	if ws == nil {
		return Plan{}, errors.New("change: workspace is required")
	}
	var mutations []Mutation
	var directories []string
	var diff strings.Builder
	for _, operation := range patch.Operations {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		switch operation.Kind {
		case PatchAdd:
			path, err := ws.ResolveCreate(operation.Path)
			if err != nil {
				return Plan{}, err
			}
			parents, err := missingParentDirectories(path)
			if err != nil {
				return Plan{}, err
			}
			directories = append(directories, parents...)
			if int64(len(operation.Data)) > s.config.MaxFileBytes {
				return Plan{}, errors.New("change: file byte limit exceeded")
			}
			before, err := s.readState(path)
			if err != nil {
				return Plan{}, err
			}
			mode := os.FileMode(0o600)
			if before.Exists {
				if before.SymlinkTarget != "" || !before.Mode.IsRegular() {
					return Plan{}, errors.New("change: patches require regular files")
				}
				mode = before.Mode
			}
			after := regularState(path, []byte(operation.Data), mode)
			mutations = append(mutations, Mutation{operation.Path, path, before, after})
			diff.WriteString(unifiedDiff(ws.Root(), before, after))
		case PatchUpdate:
			path, err := ws.ResolveRead(operation.Path)
			if err != nil {
				return Plan{}, fmt.Errorf("change: source %q is missing: %w", operation.Path, err)
			}
			before, err := s.readState(path)
			if err != nil {
				return Plan{}, err
			}
			if before.SymlinkTarget != "" || !before.Mode.IsRegular() {
				return Plan{}, errors.New("change: patches require regular files")
			}
			data, err := applyHunks(before.Data, operation.Hunks)
			if err != nil {
				return Plan{}, fmt.Errorf("change: update %q: %w", operation.Path, err)
			}
			if int64(len(data)) > s.config.MaxFileBytes {
				return Plan{}, errors.New("change: file byte limit exceeded")
			}
			after := regularState(path, data, before.Mode)
			mutations = append(mutations, Mutation{operation.Path, path, before, after})
			diff.WriteString(unifiedDiff(ws.Root(), before, after))
		case PatchDelete:
			path, err := ws.ResolveRead(operation.Path)
			if err != nil {
				return Plan{}, fmt.Errorf("change: source %q is missing: %w", operation.Path, err)
			}
			before, err := s.readState(path)
			if err != nil {
				return Plan{}, err
			}
			if before.SymlinkTarget != "" || !before.Mode.IsRegular() {
				return Plan{}, errors.New("change: patches require regular files")
			}
			after := FileState{Path: path}
			mutations = append(mutations, Mutation{operation.Path, path, before, after})
			diff.WriteString(unifiedDiff(ws.Root(), before, after))
		}
	}
	sort.Slice(mutations, func(i, j int) bool { return mutations[i].Path < mutations[j].Path })
	directories = uniquePaths(directories)
	return Plan{Mutations: mutations, Directories: directories, Diff: diff.String()}, nil
}

func applyHunks(data []byte, hunks []PatchHunk) ([]byte, error) {
	bom := []byte(nil)
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		bom = []byte{0xef, 0xbb, 0xbf}
		data = data[len(bom):]
	}
	lineEnding := "\n"
	if bytes.Contains(data, []byte("\r\n")) && !bytes.Contains(bytes.ReplaceAll(data, []byte("\r\n"), nil), []byte("\n")) {
		lineEnding = "\r\n"
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type replacement struct {
		start int
		old   int
		lines []string
	}
	var replacements []replacement
	lineIndex := 0
	for _, hunk := range hunks {
		if hunk.Context != "" {
			contextIndex, err := seekPatchSequence(lines, []string{hunk.Context}, lineIndex, false)
			if err != nil {
				return nil, fmt.Errorf("hunk context %q: %w", hunk.Context, err)
			}
			lineIndex = contextIndex + 1
		}
		var oldLines, newLines []string
		for _, line := range hunk.Lines {
			if line.Kind != '+' {
				oldLines = append(oldLines, line.Text)
			}
			if line.Kind != '-' {
				newLines = append(newLines, line.Text)
			}
		}
		if len(oldLines) == 0 {
			replacements = append(replacements, replacement{start: len(lines), lines: newLines})
			continue
		}
		found, err := seekPatchSequence(lines, oldLines, lineIndex, hunk.EndOfFile)
		if err != nil {
			return nil, err
		}
		replacements = append(replacements, replacement{start: found, old: len(oldLines), lines: newLines})
		lineIndex = found + len(oldLines)
	}
	sort.SliceStable(replacements, func(i, j int) bool {
		return replacements[i].start < replacements[j].start
	})
	result := append([]string(nil), lines...)
	for i := len(replacements) - 1; i >= 0; i-- {
		replacement := replacements[i]
		tail := append([]string(nil), result[replacement.start+replacement.old:]...)
		result = append(result[:replacement.start], replacement.lines...)
		result = append(result, tail...)
	}
	var output string
	if len(result) > 0 {
		output = strings.Join(result, lineEnding) + lineEnding
	}
	return append(bom, []byte(output)...), nil
}

// seekPatchSequence locates the single place pattern occurs at or after start.
// Comparators are tried from strictest to loosest and the first one to match
// anything decides the outcome, so a block that is exact in one place is not
// dragged into ambiguity by a looser pass. Matching more than once is an error
// rather than a silent pick of the first site: the caller must include more
// surrounding lines to say which occurrence it meant.
func seekPatchSequence(lines, pattern []string, start int, endOfFile bool) (int, error) {
	if len(pattern) == 0 || start < 0 || start > len(lines) {
		return -1, failedPatchMatchError(pattern)
	}
	comparators := []func(string, string) bool{
		func(a, b string) bool { return a == b },
		func(a, b string) bool {
			return strings.TrimRightFunc(a, isPatchSpace) == strings.TrimRightFunc(b, isPatchSpace)
		},
		func(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) },
		func(a, b string) bool {
			return normalizePatchUnicode(strings.TrimSpace(a)) == normalizePatchUnicode(strings.TrimSpace(b))
		},
	}
	for _, equal := range comparators {
		if endOfFile {
			candidate := len(lines) - len(pattern)
			if candidate >= start && patchSequenceEqual(lines[candidate:], pattern, equal) {
				return candidate, nil
			}
		}
		found, count := -1, 0
		for i := start; i <= len(lines)-len(pattern); i++ {
			if !patchSequenceEqual(lines[i:], pattern, equal) {
				continue
			}
			count++
			if found < 0 {
				found = i
			}
		}
		if count > 1 {
			return -1, fmt.Errorf("%w: found %d matches, include more surrounding lines", ErrConflict, count)
		}
		if count == 1 {
			return found, nil
		}
	}
	return -1, failedPatchMatchError(pattern)
}

func failedPatchMatchError(pattern []string) error {
	const maxMatchBytes = 1 << 10
	match := strings.Join(pattern, "\n")
	if len(match) > maxMatchBytes {
		omitted := len(match) - maxMatchBytes
		return fmt.Errorf("%w: failed to find expected lines %q (... %d bytes omitted)", ErrConflict, match[:maxMatchBytes], omitted)
	}
	return fmt.Errorf("%w: failed to find expected lines %q", ErrConflict, match)
}

func patchSequenceEqual(lines, pattern []string, equal func(string, string) bool) bool {
	if len(lines) < len(pattern) {
		return false
	}
	for i := range pattern {
		if !equal(lines[i], pattern[i]) {
			return false
		}
	}
	return true
}

func isPatchSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }

func normalizePatchUnicode(value string) string {
	replacer := strings.NewReplacer(
		"‘", "'", "’", "'", "‚", "'", "‛", "'",
		"“", "\"", "”", "\"", "„", "\"", "‟", "\"",
		"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-",
		"…", "...", "\u00a0", " ",
	)
	return replacer.Replace(value)
}

func missingParentDirectories(path string) ([]string, error) {
	var reversed []string
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return nil, errors.New("change: destination parent is not a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		reversed = append(reversed, parent)
		next := filepath.Dir(parent)
		if next == parent {
			return nil, errors.New("change: destination has no existing parent")
		}
	}
	directories := make([]string, len(reversed))
	for i := range reversed {
		directories[len(reversed)-1-i] = reversed[i]
	}
	return directories, nil
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}
