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
	writeDiffHunks(&out, shortestEditScript(splitDiffLines(before.Data), splitDiffLines(after.Data)))
	return out.String()
}

type diffEdit struct {
	kind    byte
	text    string
	oldLine int
	newLine int
}

func shortestEditScript(oldLines, newLines []string) []diffEdit {
	maximum := len(oldLines) + len(newLines)
	const maximumEditDistance = 1024
	if maximum > maximumEditDistance {
		maximum = maximumEditDistance
	}
	offset := maximum + 1
	furthest := make([]int, 2*maximum+3)
	furthest[offset+1] = 0
	trace := make([][]int, 0, maximum+1)
	for distance := 0; distance <= maximum; distance++ {
		trace = append(trace, append([]int(nil), furthest...))
		for diagonal := -distance; diagonal <= distance; diagonal += 2 {
			index := offset + diagonal
			x := 0
			if diagonal == -distance || diagonal != distance && furthest[index-1] < furthest[index+1] {
				x = furthest[index+1]
			} else {
				x = furthest[index-1] + 1
			}
			y := x - diagonal
			for x >= 0 && y >= 0 && x < len(oldLines) && y < len(newLines) && oldLines[x] == newLines[y] {
				x++
				y++
			}
			furthest[index] = x
			if x >= len(oldLines) && y >= len(newLines) {
				return backtrackEdits(oldLines, newLines, trace, distance, offset)
			}
		}
	}
	return fallbackEditScript(oldLines, newLines)
}

func fallbackEditScript(oldLines, newLines []string) []diffEdit {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-suffix-1] == newLines[len(newLines)-suffix-1] {
		suffix++
	}
	edits := make([]diffEdit, 0, len(oldLines)+len(newLines))
	for index := 0; index < prefix; index++ {
		edits = append(edits, diffEdit{kind: ' ', text: oldLines[index], oldLine: index + 1, newLine: index + 1})
	}
	for index := prefix; index < len(oldLines)-suffix; index++ {
		edits = append(edits, diffEdit{kind: '-', text: oldLines[index], oldLine: index + 1, newLine: prefix + 1})
	}
	for index := prefix; index < len(newLines)-suffix; index++ {
		edits = append(edits, diffEdit{kind: '+', text: newLines[index], oldLine: len(oldLines) - suffix + 1, newLine: index + 1})
	}
	for index := 0; index < suffix; index++ {
		oldIndex := len(oldLines) - suffix + index
		newIndex := len(newLines) - suffix + index
		edits = append(edits, diffEdit{kind: ' ', text: oldLines[oldIndex], oldLine: oldIndex + 1, newLine: newIndex + 1})
	}
	return edits
}

func backtrackEdits(oldLines, newLines []string, trace [][]int, distance, offset int) []diffEdit {
	x, y := len(oldLines), len(newLines)
	reversed := make([]diffEdit, 0, x+y)
	for current := distance; current > 0; current-- {
		furthest := trace[current]
		diagonal := x - y
		previousDiagonal := diagonal - 1
		if diagonal == -current || diagonal != current && furthest[offset+diagonal-1] < furthest[offset+diagonal+1] {
			previousDiagonal = diagonal + 1
		}
		previousX := furthest[offset+previousDiagonal]
		previousY := previousX - previousDiagonal
		for x > previousX && y > previousY {
			x--
			y--
			reversed = append(reversed, diffEdit{kind: ' ', text: oldLines[x], oldLine: x + 1, newLine: y + 1})
		}
		if x == previousX {
			y--
			reversed = append(reversed, diffEdit{kind: '+', text: newLines[y], oldLine: x + 1, newLine: y + 1})
		} else {
			x--
			reversed = append(reversed, diffEdit{kind: '-', text: oldLines[x], oldLine: x + 1, newLine: y + 1})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		reversed = append(reversed, diffEdit{kind: ' ', text: oldLines[x], oldLine: x + 1, newLine: y + 1})
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func writeDiffHunks(out *strings.Builder, edits []diffEdit) {
	const contextLines = 3
	for index := 0; index < len(edits); {
		for index < len(edits) && edits[index].kind == ' ' {
			index++
		}
		if index == len(edits) {
			return
		}
		start := max(0, index-contextLines)
		lastChange := index
		for next := index + 1; next < len(edits); next++ {
			if edits[next].kind == ' ' {
				continue
			}
			if next-lastChange-1 > contextLines*2 {
				break
			}
			lastChange = next
		}
		end := min(len(edits), lastChange+contextLines+1)
		oldCount, newCount := 0, 0
		for _, edit := range edits[start:end] {
			if edit.kind != '+' {
				oldCount++
			}
			if edit.kind != '-' {
				newCount++
			}
		}
		oldStart, newStart := edits[start].oldLine, edits[start].newLine
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, edit := range edits[start:end] {
			out.WriteByte(edit.kind)
			out.WriteString(edit.text)
			out.WriteByte('\n')
		}
		index = end
	}
}

func splitDiffLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
