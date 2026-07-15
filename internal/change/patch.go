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

// ParsePatch parses the complete OpenCode patch envelope. It intentionally
// rejects prose, blank lines between operations, and any unconsumed input.
func ParsePatch(text string) (Patch, error) {
	if strings.ContainsRune(text, 0) {
		return Patch{}, fmt.Errorf("%w: NUL byte", ErrInvalidPatch)
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return Patch{}, fmt.Errorf("%w: missing exact patch envelope", ErrInvalidPatch)
	}
	var patch Patch
	for i := 1; i < len(lines)-1; {
		kind, path, ok := parseOperationHeader(lines[i])
		if !ok {
			return Patch{}, fmt.Errorf("%w at line %d: expected operation header", ErrInvalidPatch, i+1)
		}
		if err := validPatchPath(path); err != nil {
			return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
		}
		op := PatchOperation{Kind: kind, Path: path}
		i++
		switch kind {
		case PatchAdd:
			var data strings.Builder
			for i < len(lines)-1 && !isOperationHeader(lines[i]) {
				if !strings.HasPrefix(lines[i], "+") {
					return Patch{}, fmt.Errorf("%w at line %d: add lines must start with +", ErrInvalidPatch, i+1)
				}
				data.WriteString(lines[i][1:])
				data.WriteByte('\n')
				i++
			}
			op.Data = data.String()
		case PatchDelete:
			if i < len(lines)-1 && !isOperationHeader(lines[i]) {
				return Patch{}, fmt.Errorf("%w at line %d: delete operation has content", ErrInvalidPatch, i+1)
			}
		case PatchUpdate:
			if i < len(lines)-1 && strings.HasPrefix(lines[i], "*** Move to: ") {
				op.MoveTo = strings.TrimPrefix(lines[i], "*** Move to: ")
				if err := validPatchPath(op.MoveTo); err != nil {
					return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
				}
				i++
			}
			for i < len(lines)-1 && !isOperationHeader(lines[i]) {
				if !strings.HasPrefix(lines[i], "@@") {
					return Patch{}, fmt.Errorf("%w at line %d: expected hunk header", ErrInvalidPatch, i+1)
				}
				context := strings.TrimSpace(strings.TrimPrefix(lines[i], "@@"))
				i++
				hunk := PatchHunk{Context: context}
				for i < len(lines)-1 && !isOperationHeader(lines[i]) && !strings.HasPrefix(lines[i], "@@") {
					line := lines[i]
					if line == "*** End of File" {
						hunk.EndOfFile = true
						i++
						break
					}
					if line == "" || line[0] != ' ' && line[0] != '+' && line[0] != '-' {
						return Patch{}, fmt.Errorf("%w at line %d: hunk lines require space, +, or -", ErrInvalidPatch, i+1)
					}
					hunk.Lines = append(hunk.Lines, PatchLine{line[0], line[1:]})
					i++
				}
				if len(hunk.Lines) == 0 && hunk.Context == "" {
					return Patch{}, fmt.Errorf("%w: empty update hunk", ErrInvalidPatch)
				}
				op.Hunks = append(op.Hunks, hunk)
				if hunk.EndOfFile && i < len(lines)-1 && !isOperationHeader(lines[i]) {
					return Patch{}, fmt.Errorf("%w at line %d: content follows end-of-file marker", ErrInvalidPatch, i+1)
				}
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

func parseOperationHeader(line string) (PatchOperationKind, string, bool) {
	for prefix, kind := range map[string]PatchOperationKind{
		"*** Add File: ": PatchAdd, "*** Update File: ": PatchUpdate, "*** Delete File: ": PatchDelete,
	} {
		if strings.HasPrefix(line, prefix) {
			path := strings.TrimPrefix(line, prefix)
			return kind, path, path != ""
		}
	}
	return "", "", false
}

func isOperationHeader(line string) bool {
	_, _, ok := parseOperationHeader(line)
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
			if _, err := os.Lstat(path); err == nil {
				return Plan{}, fmt.Errorf("change: add destination %q already exists", operation.Path)
			} else if !errors.Is(err, os.ErrNotExist) {
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
			before := absentState(path)
			after := regularState(path, []byte(operation.Data), 0o600)
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
			if _, err := os.Lstat(destination); err == nil {
				return Plan{}, fmt.Errorf("change: move destination %q already exists", operation.MoveTo)
			} else if !errors.Is(err, os.ErrNotExist) {
				return Plan{}, err
			}
			parents, err := missingParentDirectories(destination)
			if err != nil {
				return Plan{}, err
			}
			directories = append(directories, parents...)
			sourceAfter := absentState(path)
			destinationBefore := absentState(destination)
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
