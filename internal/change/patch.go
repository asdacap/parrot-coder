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
			count := 0
			for i < len(lines)-1 && !isOperationHeader(lines[i]) {
				if !strings.HasPrefix(lines[i], "+") {
					return Patch{}, fmt.Errorf("%w at line %d: add lines must start with +", ErrInvalidPatch, i+1)
				}
				data.WriteString(lines[i][1:])
				data.WriteByte('\n')
				count++
				i++
			}
			if count == 0 {
				return Patch{}, fmt.Errorf("%w: add operation has no content", ErrInvalidPatch)
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
				i++
				hunk := PatchHunk{}
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
				if len(hunk.Lines) == 0 {
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
			if err := requireExistingParent(path); err != nil {
				return Plan{}, err
			}
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
			if err := requireExistingParent(destination); err != nil {
				return Plan{}, err
			}
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
	return Plan{Mutations: mutations, Diff: diff.String()}, nil
}

func applyHunks(data []byte, hunks []PatchHunk) ([]byte, error) {
	result := append([]byte(nil), data...)
	for _, hunk := range hunks {
		var old, replacement strings.Builder
		for _, line := range hunk.Lines {
			if line.Kind != '+' {
				old.WriteString(line.Text)
				old.WriteByte('\n')
			}
			if line.Kind != '-' {
				replacement.WriteString(line.Text)
				replacement.WriteByte('\n')
			}
		}
		oldText, replacementText := normalizeEditNewlines(result, old.String(), replacement.String())
		needle, value := []byte(oldText), []byte(replacementText)
		count := bytes.Count(result, needle)
		if (count == 0 || hunk.EndOfFile && !bytes.HasSuffix(result, needle)) && len(needle) > 0 && needle[len(needle)-1] == '\n' {
			lineEnding := []byte("\n")
			if bytes.HasSuffix(needle, []byte("\r\n")) {
				lineEnding = []byte("\r\n")
			}
			trimmedNeedle := bytes.TrimSuffix(needle, lineEnding)
			trimmedValue := bytes.TrimSuffix(value, lineEnding)
			trimmedCount := bytes.Count(result, trimmedNeedle)
			if bytes.HasSuffix(result, trimmedNeedle) && (hunk.EndOfFile || trimmedCount == 1) {
				needle, value, count = trimmedNeedle, trimmedValue, trimmedCount
			}
		}
		if hunk.EndOfFile {
			if len(needle) == 0 || !bytes.HasSuffix(result, needle) {
				return nil, fmt.Errorf("%w: hunk does not match end of file", ErrConflict)
			}
			result = append(append([]byte(nil), result[:len(result)-len(needle)]...), value...)
			continue
		}
		if len(needle) == 0 || count != 1 {
			return nil, fmt.Errorf("%w: hunk preimage found %d times", ErrConflict, count)
		}
		result = bytes.Replace(result, needle, value, 1)
	}
	return result, nil
}
