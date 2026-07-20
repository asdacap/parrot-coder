package change

import (
	"fmt"
	"strconv"
	"strings"
)

// Header lines a unified diff may carry that say nothing about content.
var unifiedNoisePrefixes = []string{
	"diff --git ", "diff ", "index ", "new file mode ", "deleted file mode ",
	"old mode ", "new mode ", "similarity index ", "dissimilarity index ",
}

// Directives this parser deliberately refuses rather than half-supports.
var unifiedRejectedPrefixes = map[string]string{
	"rename from":      "renames are not supported; express the change as a delete and a create",
	"rename to":        "renames are not supported; express the change as a delete and a create",
	"copy from":        "copies are not supported; express the change as a create",
	"copy to":          "copies are not supported; express the change as a create",
	"GIT binary patch": "binary patches are not supported",
}

// ParseUnifiedDiff parses unified (git) diff text into the same Patch value the
// aider parser produces, so both formats share the whole apply path. The
// @@ line numbers are treated as hints only: hunks are located by matching
// their context and removed lines, exactly as aider blocks are.
//
// A source of /dev/null creates the file and a target of /dev/null deletes it.
// Renames, copies and binary patches are rejected.
//
// The "\ No newline at end of file" marker is accepted and ignored: applyHunks
// always writes a trailing newline, so a diff whose only change is the presence
// of a final newline is a no-op.
func ParseUnifiedDiff(text string) (Patch, error) {
	lines, err := patchLines(text)
	if err != nil {
		return Patch{}, err
	}
	var patch Patch
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		// A "\ No newline" marker trails the last hunk line, so it surfaces
		// here whenever the hunk header counts already accounted for that line.
		if trimmed == "" || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, `\`) || hasAnyPrefix(trimmed, unifiedNoisePrefixes) {
			i++
			continue
		}
		for prefix, reason := range unifiedRejectedPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				return Patch{}, fmt.Errorf("%w at line %d: %s", ErrInvalidPatch, i+1, reason)
			}
		}
		if !strings.HasPrefix(line, "--- ") {
			return Patch{}, fmt.Errorf("%w at line %d: expected a --- file header, found %q", ErrInvalidPatch, i+1, trimmed)
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
			return Patch{}, fmt.Errorf("%w at line %d: --- header is missing its +++ line", ErrInvalidPatch, i+1)
		}
		source, err := unifiedPath(strings.TrimPrefix(line, "--- "))
		if err != nil {
			return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
		}
		target, err := unifiedPath(strings.TrimPrefix(lines[i+1], "+++ "))
		if err != nil {
			return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+2, err)
		}
		operation, err := unifiedOperation(source, target)
		if err != nil {
			return Patch{}, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, i+1, err)
		}
		i += 2
		for i < len(lines) && strings.HasPrefix(lines[i], "@@") {
			hunk, next, err := parseUnifiedHunk(lines, i)
			if err != nil {
				return Patch{}, err
			}
			if err := appendUnifiedHunk(&operation, hunk, i+1); err != nil {
				return Patch{}, err
			}
			i = next
		}
		patch.Operations = append(patch.Operations, operation)
	}
	if len(patch.Operations) == 0 {
		return Patch{}, fmt.Errorf("%w: patch has no file headers", ErrInvalidPatch)
	}
	return finalizePatch(patch)
}

func unifiedOperation(source, target string) (PatchOperation, error) {
	switch {
	case source == "" && target == "":
		return PatchOperation{}, fmt.Errorf("file header names no path")
	case source == "":
		return PatchOperation{Kind: PatchAdd, Path: target}, validPatchPath(target)
	case target == "":
		return PatchOperation{Kind: PatchDelete, Path: source}, validPatchPath(source)
	case source != target:
		return PatchOperation{}, fmt.Errorf("source %q and target %q differ; renames are not supported", source, target)
	default:
		return PatchOperation{Kind: PatchUpdate, Path: source}, validPatchPath(source)
	}
}

// appendUnifiedHunk folds one parsed hunk into the operation, enforcing that a
// creation carries added lines only. Deletions discard their hunks: the planner
// reads the real before-state instead of trusting the patch.
func appendUnifiedHunk(operation *PatchOperation, hunk PatchHunk, line int) error {
	switch operation.Kind {
	case PatchAdd:
		for _, entry := range hunk.Lines {
			if entry.Kind != '+' {
				return fmt.Errorf("%w at line %d: file creation for %q may only add lines", ErrInvalidPatch, line, operation.Path)
			}
			operation.Data += entry.Text + "\n"
		}
	case PatchUpdate:
		operation.Hunks = append(operation.Hunks, hunk)
	}
	return nil
}

// parseUnifiedHunk reads the header at lines[start] and its body, returning the
// index of the first line after the hunk. The header counts bound the body; a
// section boundary also ends it, so a patch with miscounted headers still
// parses rather than swallowing the next file.
func parseUnifiedHunk(lines []string, start int) (PatchHunk, int, error) {
	oldCount, newCount, err := parseUnifiedHunkHeader(lines[start])
	if err != nil {
		return PatchHunk{}, 0, fmt.Errorf("%w at line %d: %v", ErrInvalidPatch, start+1, err)
	}
	var hunk PatchHunk
	old, added := 0, 0
	i := start + 1
	for i < len(lines) && (old < oldCount || added < newCount) {
		line := lines[i]
		if isUnifiedBoundary(lines, i) {
			break
		}
		i++
		switch {
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" - accepted and ignored.
		case line == "":
			// Trailing whitespace is routinely stripped from empty context lines.
			hunk.Lines = append(hunk.Lines, PatchLine{Kind: ' ', Text: ""})
			old++
			added++
		case line[0] == ' ':
			hunk.Lines = append(hunk.Lines, PatchLine{Kind: ' ', Text: line[1:]})
			old++
			added++
		case line[0] == '-':
			hunk.Lines = append(hunk.Lines, PatchLine{Kind: '-', Text: line[1:]})
			old++
		case line[0] == '+':
			hunk.Lines = append(hunk.Lines, PatchLine{Kind: '+', Text: line[1:]})
			added++
		default:
			return PatchHunk{}, 0, fmt.Errorf("%w at line %d: hunk line %q has no ' ', '+' or '-' prefix", ErrInvalidPatch, i, line)
		}
	}
	if len(hunk.Lines) == 0 {
		return PatchHunk{}, 0, fmt.Errorf("%w at line %d: hunk has no lines", ErrInvalidPatch, start+1)
	}
	return hunk, i, nil
}

// isUnifiedBoundary reports whether lines[i] starts a new hunk or file section.
func isUnifiedBoundary(lines []string, i int) bool {
	line := lines[i]
	if strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "diff --git ") {
		return true
	}
	return strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")
}

func parseUnifiedHunkHeader(line string) (oldCount, newCount int, err error) {
	rest, ok := strings.CutPrefix(line, "@@ ")
	if !ok {
		return 0, 0, fmt.Errorf("hunk header %q is malformed", line)
	}
	end := strings.Index(rest, "@@")
	if end < 0 {
		return 0, 0, fmt.Errorf("hunk header %q is missing its closing @@", line)
	}
	ranges := strings.Fields(rest[:end])
	if len(ranges) != 2 || !strings.HasPrefix(ranges[0], "-") || !strings.HasPrefix(ranges[1], "+") {
		return 0, 0, fmt.Errorf("hunk header %q needs a -old and a +new range", line)
	}
	if oldCount, err = unifiedRangeCount(ranges[0][1:]); err != nil {
		return 0, 0, err
	}
	if newCount, err = unifiedRangeCount(ranges[1][1:]); err != nil {
		return 0, 0, err
	}
	return oldCount, newCount, nil
}

// unifiedRangeCount returns the line count of a "start,count" range, which
// defaults to one line when the count is omitted.
func unifiedRangeCount(value string) (int, error) {
	start, count, found := strings.Cut(value, ",")
	if _, err := strconv.Atoi(start); err != nil {
		return 0, fmt.Errorf("range %q has no start line", value)
	}
	if !found {
		return 1, nil
	}
	parsed, err := strconv.Atoi(count)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("range %q has no line count", value)
	}
	return parsed, nil
}

// unifiedPath strips the timestamp and the a/ or b/ prefix git adds, returning
// an empty path for /dev/null.
func unifiedPath(value string) (string, error) {
	if index := strings.IndexByte(value, '\t'); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, `"`) {
		return "", fmt.Errorf("quoted path %s is not supported", value)
	}
	if value == "/dev/null" {
		return "", nil
	}
	for _, prefix := range []string{"a/", "b/"} {
		if after, ok := strings.CutPrefix(value, prefix); ok {
			return after, nil
		}
	}
	return value, nil
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
