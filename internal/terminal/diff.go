package terminal

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

const (
	maxRenderedDiffRows  = 100
	maxDiffSourceBytes   = 512 * 1024
	minSideBySideColumns = 40
)

type diffCell struct {
	line int64
	text string
	op   gitdiff.LineOp
	set  bool
}

type diffRow struct {
	left    diffCell
	right   diffCell
	heading string
}

type inlineDiffRow struct {
	oldLine int64
	newLine int64
	text    string
	op      gitdiff.LineOp
	heading string
}

// formatDiff lays out a unified diff without introducing terminal escape
// sequences. Styling remains metadata owned by the renderer.
func formatDiff(raw string, columns int, inline bool) richRows {
	clean, sourceOmitted := boundedDiffSource(raw)
	clean = strings.TrimRight(Sanitize(clean), "\n")
	if strings.TrimSpace(clean) == "" {
		return richRows{}
	}
	files, preamble, err := gitdiff.Parse(strings.NewReader(clean + "\n"))
	if err != nil || len(files) == 0 || strings.TrimSpace(preamble) != "" {
		return boundedUnifiedDiff(clean, columns, sourceOmitted)
	}
	for _, file := range files {
		if file.IsBinary || len(file.TextFragments) == 0 {
			return boundedUnifiedDiff(clean, columns, sourceOmitted)
		}
	}
	if inline {
		return renderInlineDiff(files, columns, sourceOmitted)
	}
	if columns < minSideBySideColumns {
		return boundedUnifiedDiff(clean, columns, sourceOmitted)
	}

	rows := make([]diffRow, 0)
	for _, file := range files {
		rows = append(rows, diffRow{heading: diffFileHeading(file.OldName, file.NewName)})
		for _, fragment := range file.TextFragments {
			rows = append(rows, diffRow{heading: fmt.Sprintf("@@ -%d,%d +%d,%d @@", fragment.OldPosition, fragment.OldLines, fragment.NewPosition, fragment.NewLines)})
			rows = append(rows, alignedFragmentRows(fragment)...)
		}
	}
	if len(rows) == 0 {
		return boundedUnifiedDiff(clean, columns, sourceOmitted)
	}
	result := renderSideBySide(rows, columns)
	if len(result.rows) == 0 {
		return boundedUnifiedDiff(clean, columns, sourceOmitted)
	}
	if sourceOmitted && len(rows) <= maxRenderedDiffRows {
		result.rows = append(result.rows, truncateWidth("… additional diff text omitted", columns))
		result.spans = append(result.spans, nil)
	}
	return result
}

func boundedDiffSource(raw string) (string, bool) {
	if len(raw) <= maxDiffSourceBytes {
		return raw, false
	}
	end := maxDiffSourceBytes
	for end > 0 && raw[end]&0xc0 == 0x80 {
		end--
	}
	return raw[:end], true
}

func diffFileHeading(oldName, newName string) string {
	if oldName == newName {
		return oldName
	}
	return oldName + " → " + newName
}

func alignedFragmentRows(fragment *gitdiff.TextFragment) []diffRow {
	rows := make([]diffRow, 0, len(fragment.Lines))
	oldLine, newLine := fragment.OldPosition, fragment.NewPosition
	for index := 0; index < len(fragment.Lines); {
		line := fragment.Lines[index]
		if line.Op == gitdiff.OpContext {
			text := trimDiffLine(line.Line)
			rows = append(rows, diffRow{
				left:  diffCell{line: oldLine, text: text, op: line.Op, set: true},
				right: diffCell{line: newLine, text: text, op: line.Op, set: true},
			})
			oldLine++
			newLine++
			index++
			continue
		}

		var removed, added []diffCell
		for index < len(fragment.Lines) && fragment.Lines[index].Op != gitdiff.OpContext {
			change := fragment.Lines[index]
			switch change.Op {
			case gitdiff.OpDelete:
				removed = append(removed, diffCell{line: oldLine, text: trimDiffLine(change.Line), op: change.Op, set: true})
				oldLine++
			case gitdiff.OpAdd:
				added = append(added, diffCell{line: newLine, text: trimDiffLine(change.Line), op: change.Op, set: true})
				newLine++
			}
			index++
		}
		for pair := 0; pair < max(len(removed), len(added)); pair++ {
			var row diffRow
			if pair < len(removed) {
				row.left = removed[pair]
			}
			if pair < len(added) {
				row.right = added[pair]
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func trimDiffLine(line string) string {
	return strings.TrimSuffix(line, "\n")
}

func renderInlineDiff(files []*gitdiff.File, columns int, sourceOmitted bool) richRows {
	rows := make([]inlineDiffRow, 0)
	lineDigits := 1
	for _, file := range files {
		rows = append(rows, inlineDiffRow{heading: diffFileHeading(file.OldName, file.NewName)})
		for _, fragment := range file.TextFragments {
			rows = append(rows, inlineDiffRow{heading: fmt.Sprintf("@@ -%d,%d +%d,%d @@", fragment.OldPosition, fragment.OldLines, fragment.NewPosition, fragment.NewLines)})
			oldLine, newLine := fragment.OldPosition, fragment.NewPosition
			for _, line := range fragment.Lines {
				row := inlineDiffRow{text: trimDiffLine(line.Line), op: line.Op}
				switch line.Op {
				case gitdiff.OpContext:
					row.oldLine, row.newLine = oldLine, newLine
					oldLine++
					newLine++
				case gitdiff.OpDelete:
					row.oldLine = oldLine
					oldLine++
				case gitdiff.OpAdd:
					row.newLine = newLine
					newLine++
				}
				lineDigits = max(lineDigits, len(strconv.FormatInt(max(row.oldLine, row.newLine), 10)))
				rows = append(rows, row)
			}
		}
	}

	if columns <= 0 {
		columns = 1
	}
	limit := min(len(rows), maxRenderedDiffRows)
	result := richRows{rows: make([]string, 0, limit+1), spans: make([][]textSpan, 0, limit+1)}
	for _, row := range rows[:limit] {
		if row.heading != "" {
			result.rows = append(result.rows, truncateWidth(row.heading, columns))
			result.spans = append(result.spans, []textSpan{{start: 0, end: len(result.rows[len(result.rows)-1]), style: ansiStyle{dim: true}}})
			continue
		}
		lineNumber, marker := "", " "
		if row.op == gitdiff.OpDelete {
			lineNumber = strconv.FormatInt(row.oldLine, 10)
			marker = "-"
		} else {
			lineNumber = strconv.FormatInt(row.newLine, 10)
			if row.op == gitdiff.OpAdd {
				marker = "+"
			}
		}
		prefix := fmt.Sprintf("%*s %s", lineDigits, lineNumber, marker)
		line := truncateWidth(prefix+expandDiffTabs(row.text), columns)
		result.rows = append(result.rows, line)
		var style ansiStyle
		if row.op == gitdiff.OpDelete {
			style.color = "31"
		} else if row.op == gitdiff.OpAdd {
			style.color = "32"
		}
		if style == (ansiStyle{}) {
			result.spans = append(result.spans, nil)
		} else {
			result.spans = append(result.spans, []textSpan{{start: 0, end: len(line), style: style}})
		}
	}
	if omitted := len(rows) - limit; omitted > 0 {
		result.rows = append(result.rows, truncateWidth(fmt.Sprintf("… %d diff rows omitted", omitted), columns))
		result.spans = append(result.spans, []textSpan{{start: 0, end: len(result.rows[len(result.rows)-1]), style: ansiStyle{dim: true}}})
	} else if sourceOmitted {
		result.rows = append(result.rows, truncateWidth("… additional diff text omitted", columns))
		result.spans = append(result.spans, nil)
	}
	return result
}

func renderSideBySide(diffRows []diffRow, columns int) richRows {
	lineDigits := 1
	for _, row := range diffRows {
		lineDigits = max(lineDigits, len(strconv.FormatInt(max(row.left.line, row.right.line), 10)))
	}
	gutter := " │ "
	cellWidth := (columns - displayWidth(gutter)) / 2
	contentWidth := cellWidth - lineDigits - 2 // line number and change marker
	if contentWidth < 4 {
		return richRows{}
	}

	limit := min(len(diffRows), maxRenderedDiffRows)
	result := richRows{rows: make([]string, 0, limit+1), spans: make([][]textSpan, 0, limit+1)}
	for _, row := range diffRows[:limit] {
		if row.heading != "" {
			result.rows = append(result.rows, truncateWidth(row.heading, columns))
			result.spans = append(result.spans, []textSpan{{start: 0, end: len(result.rows[len(result.rows)-1]), style: ansiStyle{dim: true}}})
			continue
		}
		left, leftStart := renderDiffCell(row.left, lineDigits, contentWidth)
		right, rightStart := renderDiffCell(row.right, lineDigits, contentWidth)
		line := left + gutter + right
		var spans []textSpan
		if row.left.set && row.left.op == gitdiff.OpDelete {
			spans = append(spans, textSpan{start: leftStart, end: len(left), style: ansiStyle{color: "31"}})
		}
		if row.right.set && row.right.op == gitdiff.OpAdd {
			start := len(left) + len(gutter) + rightStart
			spans = append(spans, textSpan{start: start, end: len(line), style: ansiStyle{color: "32"}})
		}
		result.rows = append(result.rows, line)
		result.spans = append(result.spans, spans)
	}
	if omitted := len(diffRows) - limit; omitted > 0 {
		result.rows = append(result.rows, clipPad(fmt.Sprintf("… %d diff rows omitted", omitted), columns))
		result.spans = append(result.spans, []textSpan{{start: 0, end: len(result.rows[len(result.rows)-1]), style: ansiStyle{dim: true}}})
	}
	return result
}

func renderDiffCell(cell diffCell, digits, contentWidth int) (string, int) {
	if !cell.set {
		return strings.Repeat(" ", digits+2+contentWidth), digits + 2
	}
	marker := " "
	if cell.op == gitdiff.OpDelete {
		marker = "-"
	} else if cell.op == gitdiff.OpAdd {
		marker = "+"
	}
	prefix := fmt.Sprintf("%*d %s", digits, cell.line, marker)
	return prefix + clipPad(expandDiffTabs(cell.text), contentWidth), len(prefix)
}

func expandDiffTabs(value string) string {
	var output strings.Builder
	column := 0
	for _, char := range value {
		if char == '\t' {
			spaces := 4 - column%4
			output.WriteString(strings.Repeat(" ", spaces))
			column += spaces
			continue
		}
		output.WriteRune(char)
		column += runeWidth(char)
	}
	return output.String()
}

func clipPad(value string, width int) string {
	value = truncateWidth(value, max(0, width))
	return value + strings.Repeat(" ", max(0, width-displayWidth(value)))
}

func boundedUnifiedDiff(raw string, columns int, sourceOmitted bool) richRows {
	if columns <= 0 {
		columns = 1
	}
	lines := strings.Split(raw, "\n")
	limit := min(len(lines), maxRenderedDiffRows)
	result := richRows{rows: make([]string, 0, limit+1), spans: make([][]textSpan, 0, limit+1)}
	for _, line := range lines[:limit] {
		row := truncateWidth(expandDiffTabs(line), columns)
		result.rows = append(result.rows, row)
		var style ansiStyle
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			style.color = "32"
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			style.color = "31"
		}
		if style == (ansiStyle{}) {
			result.spans = append(result.spans, nil)
		} else {
			result.spans = append(result.spans, []textSpan{{start: 0, end: len(row), style: style}})
		}
	}
	if omitted := len(lines) - limit; omitted > 0 {
		result.rows = append(result.rows, truncateWidth(fmt.Sprintf("… %d omitted", omitted), columns))
		result.spans = append(result.spans, nil)
	} else if sourceOmitted {
		result.rows = append(result.rows, truncateWidth("… additional diff text omitted", columns))
		result.spans = append(result.spans, nil)
	}
	return result
}
