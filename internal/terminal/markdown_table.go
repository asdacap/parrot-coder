package terminal

import "strings"

func (t markdownTableState) pending() bool {
	return len(t.candidate) > 0 || len(t.header) > 0
}

func cloneTableRows(rows [][]string) [][]string {
	clone := make([][]string, len(rows))
	for i, row := range rows {
		clone[i] = append([]string(nil), row...)
	}
	return clone
}

// parseMarkdownTableRow splits pipes outside code spans. It deliberately keeps
// Markdown escapes in the cell text so the ordinary inline renderer remains the
// single place that interprets them.
func parseMarkdownTableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.ContainsRune(line, '|') {
		return nil, false
	}
	var cells []string
	start, backticks := 0, 0
	firstPipe, lastPipe := -1, -1
	for position := 0; position < len(line); position++ {
		switch line[position] {
		case '`':
			count := 1
			for position+count < len(line) && line[position+count] == '`' {
				count++
			}
			if backticks == 0 {
				backticks = count
			} else if backticks == count {
				backticks = 0
			}
			position += count - 1
		case '|':
			if backticks != 0 || markdownDelimiterEscaped(line, position) {
				continue
			}
			if firstPipe < 0 {
				firstPipe = position
			}
			lastPipe = position
			cells = append(cells, strings.TrimSpace(line[start:position]))
			start = position + 1
		}
	}
	cells = append(cells, strings.TrimSpace(line[start:]))
	if len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	outerPipes := firstPipe == 0 && lastPipe == len(line)-1 && firstPipe != lastPipe
	return cells, len(cells) >= 2 || len(cells) == 1 && outerPipes
}

func normalizeTableRow(cells []string, columns int) []string {
	row := make([]string, columns)
	copy(row, cells)
	if len(cells) > columns {
		row[columns-1] = strings.Join(cells[columns-1:], " | ")
	}
	return row
}

func parseTableDelimiter(line string, columns int) ([]tableAlignment, bool) {
	cells, ok := parseMarkdownTableRow(line)
	if !ok || len(cells) != columns {
		return nil, false
	}
	align := make([]tableAlignment, len(cells))
	for i, cell := range cells {
		cell = strings.TrimSpace(cell)
		left, right := strings.HasPrefix(cell, ":"), strings.HasSuffix(cell, ":")
		if left {
			cell = strings.TrimPrefix(cell, ":")
		}
		if right {
			cell = strings.TrimSuffix(cell, ":")
		}
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			align[i] = tableAlignCenter
		case right:
			align[i] = tableAlignRight
		default:
			align[i] = tableAlignLeft
		}
	}
	return align, true
}

func flushMarkdownTable(state *markdownState, columns int) richRows {
	table := state.table
	state.table = markdownTableState{}
	if len(table.header) == 0 {
		if len(table.candidate) == 0 {
			return richRows{}
		}
		return layoutRichRuns(withMarkdownPrefix(table.prefix, markdownLineRuns(table.candidateLine, columns-displayWidth(table.prefix))), hangingIndent(table.prefix), columns)
	}
	return renderMarkdownTable(table, columns)
}

func renderMarkdownTable(table markdownTableState, columns int) richRows {
	available := columns - displayWidth(table.prefix)
	if available < 4*len(table.header)+1 {
		return renderNarrowMarkdownTable(table, columns)
	}
	cellRows := append([][]string{table.header}, table.rows...)
	widths, ok := tableColumnWidths(cellRows, available)
	if !ok {
		return renderNarrowMarkdownTable(table, columns)
	}
	indent := hangingIndent(table.prefix)
	var output richRows
	output.append(tableBorder(table.prefix, widths, "┌", "┬", "┐"))
	for rowIndex, cells := range cellRows {
		output.append(renderMarkdownTableRow(indent, cells, widths, table.align, rowIndex == 0))
		if rowIndex == 0 {
			output.append(tableBorder(indent, widths, "├", "┼", "┤"))
		}
	}
	output.append(tableBorder(indent, widths, "└", "┴", "┘"))
	return output
}

func renderNarrowMarkdownTable(table markdownTableState, columns int) richRows {
	if len(table.rows) == 0 {
		return layoutRichRuns(withMarkdownPrefix(table.prefix, markdownInlineRuns(strings.Join(table.header, ", "), ansiStyle{bold: true, color: "36"})), hangingIndent(table.prefix), columns)
	}
	indent := hangingIndent(table.prefix)
	var output richRows
	for rowIndex, row := range table.rows {
		for columnIndex, cell := range row {
			prefix := indent
			if rowIndex == 0 && columnIndex == 0 {
				prefix = table.prefix
			}
			runs := markdownInlineRuns(table.header[columnIndex]+": ", ansiStyle{bold: true, color: "36"})
			appendRuns(&runs, markdownInlineRuns(cell, ansiStyle{}))
			output.append(layoutRichRuns(withMarkdownPrefix(prefix, runs), indent, columns))
		}
		if rowIndex+1 < len(table.rows) {
			output.append(layoutRichRuns([]textRun{{text: indent}}, indent, columns))
		}
	}
	return output
}

func tableColumnWidths(rows [][]string, available int) ([]int, bool) {
	columns := len(rows[0])
	widths := make([]int, columns)
	minimums := make([]int, columns)
	for _, row := range rows {
		for i, cell := range row {
			plain := runsText(markdownInlineRuns(cell, ansiStyle{}))
			widths[i] = max(widths[i], max(1, expandedDisplayWidth(plain)))
			minimums[i] = max(minimums[i], max(1, widestExpandedRune(plain)))
		}
	}
	// Borders and one padding space on either side consume 3n+1 columns.
	budget := available - (3*columns + 1)
	if totalWidths(minimums) > budget {
		return nil, false
	}
	for totalWidths(widths) > budget {
		widest := -1
		for i := range widths {
			if widths[i] > minimums[i] && (widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			return nil, false
		}
		widths[widest]--
	}
	return widths, true
}

func expandedDisplayWidth(value string) int {
	width := 0
	for _, raw := range value {
		for _, expanded := range expandRune(raw) {
			width += runeWidth(expanded)
		}
	}
	return width
}

func widestExpandedRune(value string) int {
	width := 0
	for _, raw := range value {
		for _, expanded := range expandRune(raw) {
			width = max(width, runeWidth(expanded))
		}
	}
	return width
}

func totalWidths(widths []int) int {
	total := 0
	for _, width := range widths {
		total += width
	}
	return total
}

func runsText(runs []textRun) string {
	var output strings.Builder
	for _, run := range runs {
		output.WriteString(run.text)
	}
	return output.String()
}

func tableBorder(prefix string, widths []int, left, middle, right string) richRows {
	var runs []textRun
	style := ansiStyle{color: "90", dim: true}
	appendTextRun(&runs, prefix+left, style)
	for i, width := range widths {
		if i > 0 {
			appendTextRun(&runs, middle, style)
		}
		appendTextRun(&runs, strings.Repeat("─", width+2), style)
	}
	appendTextRun(&runs, right, style)
	return layoutRichRuns(runs, "", max(1, displayWidth(prefix)+totalWidths(widths)+3*len(widths)+1))
}

func renderMarkdownTableRow(prefix string, cells []string, widths []int, align []tableAlignment, header bool) richRows {
	wrapped := make([]richRows, len(cells))
	height := 1
	for i, cell := range cells {
		base := ansiStyle{}
		if header {
			base = ansiStyle{bold: true, color: "36"}
		}
		wrapped[i] = layoutRichRuns(markdownInlineRuns(cell, base), "", widths[i])
		height = max(height, len(wrapped[i].rows))
	}

	var output richRows
	for line := 0; line < height; line++ {
		var runs []textRun
		border := ansiStyle{color: "90", dim: true}
		appendTextRun(&runs, prefix+"│", border)
		for i := range cells {
			content := ""
			var spans []textSpan
			if line < len(wrapped[i].rows) {
				content = wrapped[i].rows[line]
				spans = wrapped[i].spans[line]
			}
			padding := widths[i] - displayWidth(content)
			left, right := 0, padding
			switch align[i] {
			case tableAlignRight:
				left, right = padding, 0
			case tableAlignCenter:
				left, right = padding/2, padding-padding/2
			}
			appendTextRun(&runs, " "+strings.Repeat(" ", left), ansiStyle{})
			appendRuns(&runs, runsFromRichRow(content, spans))
			appendTextRun(&runs, strings.Repeat(" ", right)+" ", ansiStyle{})
			appendTextRun(&runs, "│", border)
		}
		output.append(layoutRichRuns(runs, "", max(1, displayWidth(prefix)+totalWidths(widths)+3*len(widths)+1)))
	}
	return output
}

func runsFromRichRow(row string, spans []textSpan) []textRun {
	if len(spans) == 0 {
		return []textRun{{text: row}}
	}
	var runs []textRun
	position := 0
	for _, span := range spans {
		if span.start > position {
			appendTextRun(&runs, row[position:span.start], ansiStyle{})
		}
		appendTextRun(&runs, row[span.start:span.end], span.style)
		position = span.end
	}
	if position < len(row) {
		appendTextRun(&runs, row[position:], ansiStyle{})
	}
	return runs
}
