package change

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
)

func unifiedDiff(root string, before, after FileState) string {
	path := before.Path
	if path == "" {
		path = after.Path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)
	oldName, newName := "a/"+rel, "b/"+rel
	if !before.Exists {
		oldName = "/dev/null"
	}
	if !after.Exists {
		newName = "/dev/null"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	if bytes.IndexByte(before.Data, 0) >= 0 || bytes.IndexByte(after.Data, 0) >= 0 {
		out.WriteString("Binary files differ\n")
		return out.String()
	}
	oldLines := splitDiffLines(before.Data)
	newLines := splitDiffLines(after.Data)
	fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines))
	for _, line := range oldLines {
		out.WriteByte('-')
		out.WriteString(line)
		out.WriteByte('\n')
	}
	for _, line := range newLines {
		out.WriteByte('+')
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func splitDiffLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
