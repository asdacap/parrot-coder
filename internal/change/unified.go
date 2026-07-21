package change

import (
	"fmt"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// ParseUnifiedDiff parses unified (git) diff text into the same Patch value the
// aider parser produces, so both formats share the whole apply path.
//
// A source of /dev/null creates the file and a target of /dev/null deletes it.
// Renames, copies and binary patches are rejected.
//
// The "\ No newline at end of file" marker is accepted and ignored: applyHunks
// always writes a trailing newline, so a diff whose only change is the presence
// of a final newline is a no-op.
func ParseUnifiedDiff(text string) (Patch, error) {
	// Reject NUL bytes.
	if strings.ContainsRune(text, 0) {
		return Patch{}, fmt.Errorf("%w: NUL byte", ErrInvalidPatch)
	}

	// Reject quoted paths (not supported by the workspace resolver).
	if containsQuotedPath(text) {
		return Patch{}, fmt.Errorf("%w: quoted paths are not supported", ErrInvalidPatch)
	}

	// Reject mismatched traditional header paths.
	if err := checkMismatchedHeaderPaths(text); err != nil {
		return Patch{}, err
	}

	// Parse with go-gitdiff.
	files, _, err := gitdiff.Parse(strings.NewReader(text))
	if err != nil {
		return Patch{}, fmt.Errorf("%w: %v", ErrInvalidPatch, err)
	}

	// Convert to internal Patch type.
	var patch Patch
	for _, file := range files {
		op, err := convertFile(file)
		if err != nil {
			return Patch{}, err
		}
		patch.Operations = append(patch.Operations, op)
	}

	if len(patch.Operations) == 0 {
		return Patch{}, fmt.Errorf("%w: patch has no file headers", ErrInvalidPatch)
	}

	return finalizePatch(patch)
}

// containsQuotedPath reports whether the text contains a "--- " or "+++ " line
// whose path is quoted (starts with a double quote after stripping the header
// prefix and any a/b prefix).
func containsQuotedPath(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		for _, prefix := range []string{"--- ", "+++ "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			rest := strings.TrimSpace(line[len(prefix):])
			// Strip timestamp after tab.
			if idx := strings.IndexByte(rest, '\t'); idx >= 0 {
				rest = rest[:idx]
			}
			// Strip a/ or b/ prefix.
			for _, ab := range []string{"a/", "b/"} {
				rest = strings.TrimPrefix(rest, ab)
			}
			if strings.HasPrefix(rest, `"`) {
				return true
			}
		}
	}
	return false
}

// checkMismatchedHeaderPaths scans traditional diff headers (--- / +++) and
// reports an error when the source and target paths differ (which would
// indicate a rename in a format that does not support them).
func checkMismatchedHeaderPaths(text string) error {
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		// Skip git headers.
		if strings.HasPrefix(lines[i], "diff --git ") {
			continue
		}
		if !strings.HasPrefix(lines[i], "--- ") {
			continue
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
			continue
		}
		// Verify the next non-noise line starts with @@ - (confirmed
		// traditional header rather than stray ---/+++ lines).
		hasFragment := false
		for j := i + 2; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" || strings.HasPrefix(t, "\\") || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "diff ") {
				continue
			}
			hasFragment = strings.HasPrefix(t, "@@ -")
			break
		}
		if !hasFragment {
			continue
		}

		source := extractHeaderPath(lines[i][len("--- "):])
		target := extractHeaderPath(lines[i+1][len("+++ "):])
		if source == "" || target == "" {
			continue
		}
		if source != target {
			return fmt.Errorf("%w: source %q and target %q differ; renames are not supported",
				ErrInvalidPatch, source, target)
		}
	}
	return nil
}

// extractHeaderPath extracts the file path from a unified-diff header value,
// stripping the timestamp and the a/b prefix. Returns an empty string for
// /dev/null.
func extractHeaderPath(value string) string {
	if idx := strings.IndexByte(value, '\t'); idx >= 0 {
		value = value[:idx]
	}
	value = strings.TrimSpace(value)
	if value == "/dev/null" {
		return ""
	}
	for _, prefix := range []string{"a/", "b/"} {
		if after, ok := strings.CutPrefix(value, prefix); ok {
			return after
		}
	}
	return value
}

// cleanDiffPath strips the a/ and b/ prefixes that git's diff format may
// include on header paths.
func cleanDiffPath(path string) string {
	for _, prefix := range []string{"a/", "b/"} {
		if after, ok := strings.CutPrefix(path, prefix); ok {
			return after
		}
	}
	return path
}

// convertFile converts a go-gitdiff File into an internal PatchOperation.
func convertFile(file *gitdiff.File) (PatchOperation, error) {
	var op PatchOperation

	if file.IsRename {
		return PatchOperation{}, fmt.Errorf("%w: renames are not supported; express the change as a delete and a create", ErrInvalidPatch)
	}
	if file.IsCopy {
		return PatchOperation{}, fmt.Errorf("%w: copies are not supported; express the change as a create", ErrInvalidPatch)
	}
	if file.IsBinary {
		return PatchOperation{}, fmt.Errorf("%w: binary patches are not supported", ErrInvalidPatch)
	}

	switch {
	case file.IsNew:
		op.Kind = PatchAdd
		op.Path = cleanDiffPath(file.NewName)
		if err := validPatchPath(op.Path); err != nil {
			return PatchOperation{}, fmt.Errorf("%w: %v", ErrInvalidPatch, err)
		}
		for _, frag := range file.TextFragments {
			for _, line := range frag.Lines {
				if line.Op != gitdiff.OpAdd {
					return PatchOperation{}, fmt.Errorf("%w: file creation for %q may only add lines", ErrInvalidPatch, op.Path)
				}
				op.Data += strings.TrimSuffix(line.Line, "\n") + "\n"
			}
		}

	case file.IsDelete:
		op.Kind = PatchDelete
		op.Path = cleanDiffPath(file.OldName)
		if err := validPatchPath(op.Path); err != nil {
			return PatchOperation{}, fmt.Errorf("%w: %v", ErrInvalidPatch, err)
		}

	default:
		op.Kind = PatchUpdate
		op.Path = cleanDiffPath(file.NewName)
		if op.Path == "" {
			op.Path = cleanDiffPath(file.OldName)
		}
		if err := validPatchPath(op.Path); err != nil {
			return PatchOperation{}, fmt.Errorf("%w: %v", ErrInvalidPatch, err)
		}
		for _, frag := range file.TextFragments {
			var hunk PatchHunk
			for _, line := range frag.Lines {
				var kind byte
				switch line.Op {
				case gitdiff.OpContext:
					kind = ' '
				case gitdiff.OpDelete:
					kind = '-'
				case gitdiff.OpAdd:
					kind = '+'
				default:
					continue
				}
				hunk.Lines = append(hunk.Lines, PatchLine{Kind: kind, Text: strings.TrimSuffix(line.Line, "\n")})
			}
			if len(hunk.Lines) > 0 {
				op.Hunks = append(op.Hunks, hunk)
			}
		}
	}

	return op, nil
}
