package formatter

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrOutputLimit
	}
	return data, nil
}

func unifiedDiff(path string, before, after []byte) string {
	name := filepath.ToSlash(path)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	if bytes.IndexByte(before, 0) >= 0 || bytes.IndexByte(after, 0) >= 0 {
		out.WriteString("Binary files differ\n")
		return out.String()
	}
	oldLines := diffLines(before)
	newLines := diffLines(after)
	fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines))
	for _, line := range oldLines {
		fmt.Fprintf(&out, "-%s\n", line)
	}
	for _, line := range newLines {
		fmt.Fprintf(&out, "+%s\n", line)
	}
	return out.String()
}

func diffLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
