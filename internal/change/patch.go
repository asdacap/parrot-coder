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
	Kind   PatchOperationKind
	Path   string
	MoveTo string
	Data   string
	Hunks  []PatchHunk
}

type Patch struct{ Operations []PatchOperation }

// ParsePatch parses the Codex apply_patch format. It retains the Move File
// alias for compatibility with older Parrot callers.
func ParsePatch(text string) (Patch, error) {
	if strings.ContainsRune(text, 0) {
		return Patch{}, fmt.Errorf("%w: NUL byte", ErrInvalidPatch)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	if len(lines) >= 4 && (lines[0] == "<<EOF" || lines[0] == "<<'EOF'" || lines[0] == `<<"EOF"`) && strings.HasSuffix(lines[len(lines)-1], "EOF") {
		lines = lines[1 : len(lines)-1]
	}
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "*** Begin Patch" || strings.TrimSpace(lines[len(lines)-1]) != "*** End Patch" {
		return Patch{}, fmt.Errorf("%w: missing exact patch envelope", ErrInvalidPatch)
	}
	var patch Patch
	for i := 1; i < len(lines)-1; {
		kind, path, moveTo, ok := parseOperationHeader(strings.TrimSpace(lines[i]))
		if !ok {
			return Patch{}, fmt.Errorf("%w at line %d: expected operation header", ErrInvalidPatch, i+1)
		}
		if err := validPatchPath(path); err != nil {
			return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
		}
		if moveTo != "" {
			if err := validPatchPath(moveTo); err != nil {
				return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
			}
		}
		op := PatchOperation{Kind: kind, Path: path, MoveTo: moveTo}
		i++
		switch kind {
		case PatchAdd:
			var data strings.Builder
			for i < len(lines)-1 && !isOperationHeader(strings.TrimSpace(lines[i])) {
				if !strings.HasPrefix(lines[i], "+") {
					return Patch{}, fmt.Errorf("%w at line %d: add lines must start with +", ErrInvalidPatch, i+1)
				}
				data.WriteString(lines[i][1:])
				data.WriteByte('\n')
				i++
			}
			if data.Len() == 0 {
				return Patch{}, fmt.Errorf("%w: empty add hunk", ErrInvalidPatch)
			}
			op.Data = data.String()
		case PatchDelete:
			if i < len(lines)-1 && !isOperationHeader(strings.TrimSpace(lines[i])) {
				return Patch{}, fmt.Errorf("%w at line %d: delete operation has content", ErrInvalidPatch, i+1)
			}
		case PatchUpdate:
			if i < len(lines)-1 && strings.HasPrefix(strings.TrimSpace(lines[i]), "*** Move to: ") {
				if op.MoveTo != "" {
					return Patch{}, fmt.Errorf("%w at line %d: move destination specified twice", ErrInvalidPatch, i+1)
				}
				op.MoveTo = strings.TrimPrefix(strings.TrimSpace(lines[i]), "*** Move to: ")
				if err := validPatchPath(op.MoveTo); err != nil {
					return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
				}
				i++
			}
			var hunk *PatchHunk
			for i < len(lines)-1 && !isOperationHeader(strings.TrimSpace(lines[i])) {
				line, trimmed := lines[i], strings.TrimSpace(lines[i])
				if trimmed == "@@" || strings.HasPrefix(strings.TrimRight(line, " \t"), "@@ ") {
					if hunk != nil && len(hunk.Lines) == 0 {
						return Patch{}, fmt.Errorf("%w: empty update hunk", ErrInvalidPatch)
					}
					op.Hunks = append(op.Hunks, PatchHunk{Context: strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(line, " \t"), "@@"))})
					hunk = &op.Hunks[len(op.Hunks)-1]
					i++
					continue
				}
				if trimmed == "*** End of File" {
					if hunk == nil || len(hunk.Lines) == 0 {
						return Patch{}, fmt.Errorf("%w: empty update hunk", ErrInvalidPatch)
					}
					hunk.EndOfFile = true
					i++
					for i < len(lines)-1 && strings.TrimSpace(lines[i]) == "" {
						i++
					}
					continue
				}
				if hunk != nil && hunk.EndOfFile {
					return Patch{}, fmt.Errorf("%w at line %d: expected hunk header", ErrInvalidPatch, i+1)
				}
				if hunk == nil {
					op.Hunks = append(op.Hunks, PatchHunk{})
					hunk = &op.Hunks[len(op.Hunks)-1]
				}
				if line == "" {
					hunk.Lines = append(hunk.Lines, PatchLine{' ', ""})
				} else if line[0] == ' ' || line[0] == '+' || line[0] == '-' {
					hunk.Lines = append(hunk.Lines, PatchLine{line[0], line[1:]})
				} else {
					return Patch{}, fmt.Errorf("%w at line %d: hunk lines require space, +, or -", ErrInvalidPatch, i+1)
				}
				i++
			}
			if hunk != nil && len(hunk.Lines) == 0 {
				return Patch{}, fmt.Errorf("%w: empty update hunk", ErrInvalidPatch)
			}
			if len(op.Hunks) == 0 && op.MoveTo == "" {
				return Patch{}, fmt.Errorf("%w: update has neither hunks nor move", ErrInvalidPatch)
			}
		}
		patch.Operations = append(patch.Operations, op)
	}
	if len(patch.Operations) == 0 {
		return Patch{}, fmt.Errorf("%w: patch has no operations", ErrInvalidPatch)
	}
	if err := validatePatchPaths(patch); err != nil {
		return Patch{}, err
	}
	return patch, nil
}

func parseOperationHeader(line string) (PatchOperationKind, string, string, bool) {
	// Codex represents a move as an Update File operation followed by a Move to
	// directive. Accept the commonly generated one-line spelling as an alias so
	// patches produced from the tool's "move operations" description remain
	// interoperable. A Move File may also contain update hunks.
	if strings.HasPrefix(line, "*** Move File: ") {
		move := strings.TrimPrefix(line, "*** Move File: ")
		path, moveTo, ok := strings.Cut(move, " -> ")
		if !ok || path == "" || moveTo == "" {
			return "", "", "", false
		}
		return PatchUpdate, path, moveTo, true
	}
	for prefix, kind := range map[string]PatchOperationKind{
		"*** Add File: ": PatchAdd, "*** Update File: ": PatchUpdate, "*** Delete File: ": PatchDelete,
	} {
		if strings.HasPrefix(line, prefix) {
			path := strings.TrimPrefix(line, prefix)
			return kind, path, "", path != ""
		}
	}
	return "", "", "", false
}

func isOperationHeader(line string) bool {
	_, _, _, ok := parseOperationHeader(line)
	return ok
}

func validPatchPath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return errors.New("path must be a clean relative workspace path")
	}
	return nil
}

func validatePatchPaths(patch Patch) error {
	var paths []string
	for _, op := range patch.Operations {
		paths = append(paths, filepath.Clean(op.Path))
		if op.MoveTo != "" {
			paths = append(paths, filepath.Clean(op.MoveTo))
		}
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

func (s *Service) PlanPatch(ctx context.Context, ws *workspace.Workspace, text string) (Plan, error) {
	patch, err := ParsePatch(text)
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
		case PatchDelete, PatchUpdate:
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
			if operation.Kind == PatchDelete {
				after := absentState(path)
				mutations = append(mutations, Mutation{operation.Path, path, before, after})
				diff.WriteString(unifiedDiff(ws.Root(), before, after))
				continue
			}
			data, err := applyHunks(before.Data, operation.Hunks)
			if err != nil {
				return Plan{}, fmt.Errorf("change: update %q: %w", operation.Path, err)
			}
			if int64(len(data)) > s.config.MaxFileBytes {
				return Plan{}, errors.New("change: file byte limit exceeded")
			}
			if operation.MoveTo == "" {
				after := regularState(path, data, before.Mode)
				mutations = append(mutations, Mutation{operation.Path, path, before, after})
				diff.WriteString(unifiedDiff(ws.Root(), before, after))
				continue
			}
			destination, err := ws.ResolveCreate(operation.MoveTo)
			if err != nil {
				return Plan{}, err
			}
			parents, err := missingParentDirectories(destination)
			if err != nil {
				return Plan{}, err
			}
			directories = append(directories, parents...)
			sourceAfter := absentState(path)
			destinationBefore, err := s.readState(destination)
			if err != nil {
				return Plan{}, err
			}
			if destinationBefore.Exists && (destinationBefore.SymlinkTarget != "" || !destinationBefore.Mode.IsRegular()) {
				return Plan{}, errors.New("change: patches require regular files")
			}
			destinationAfter := regularState(destination, data, before.Mode)
			mutations = append(mutations,
				Mutation{operation.Path, path, before, sourceAfter},
				Mutation{operation.MoveTo, destination, destinationBefore, destinationAfter})
			diff.WriteString(unifiedDiff(ws.Root(), before, sourceAfter))
			diff.WriteString(unifiedDiff(ws.Root(), destinationBefore, destinationAfter))
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
			contextIndex := seekPatchSequence(lines, []string{hunk.Context}, lineIndex, false)
			if contextIndex < 0 {
				return nil, fmt.Errorf("%w: failed to find hunk context %q", ErrConflict, hunk.Context)
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
		found := seekPatchSequence(lines, oldLines, lineIndex, hunk.EndOfFile)
		if found < 0 {
			return nil, fmt.Errorf("%w: failed to find expected hunk lines", ErrConflict)
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

func seekPatchSequence(lines, pattern []string, start int, endOfFile bool) int {
	if len(pattern) == 0 || start < 0 || start > len(lines) {
		return -1
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
				return candidate
			}
		}
		for i := start; i <= len(lines)-len(pattern); i++ {
			if patchSequenceEqual(lines[i:], pattern, equal) {
				return i
			}
		}
	}
	return -1
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
