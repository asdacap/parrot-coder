package terminal

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

const (
	maxHighlightBytes = 512 * 1024
	maxHighlightLines = 10_000
	// Live previews are redrawn for every cumulative stream update. Keep their
	// source budget much smaller than the final highlighting budget so an open
	// fence or unfinished line cannot cause unbounded work before row clipping.
	maxLiveMarkdownRunes = 16 * 1024
	maxLiveMarkdownLines = 256
)

// ansiStyle is renderer-owned presentation metadata. Model text is sanitized
// before it is converted to runs, and ANSI is introduced only when a row is
// finally written to a color-capable TTY.
type ansiStyle struct {
	color     string
	bold      bool
	dim       bool
	italic    bool
	underline bool
	strike    bool
}

type textRun struct {
	text  string
	style ansiStyle
}

// textSpan uses byte offsets into one already-sanitized rendered row.
type textSpan struct {
	start int
	end   int
	style ansiStyle
}

type richRows struct {
	rows  []string
	spans [][]textSpan
}

type markdownState struct {
	inFence     bool
	fenceChar   rune
	fenceLength int
	language    string
	lexer       chroma.Lexer
	plainFence  bool
	code        []string
	codeBytes   int
}

func renderAssistantMarkdown(prefix, source string, columns int, color bool) richRows {
	prefix = Sanitize(prefix)
	clean := strings.TrimRight(Sanitize(source), "\r\n")
	lines := strings.Split(clean, "\n")
	state := markdownState{}
	started := false
	var output richRows
	for _, line := range lines {
		linePrefix := markdownPrefix(prefix, started)
		rendered := renderMarkdownLine(linePrefix, line, columns, &state, color)
		output.append(rendered)
		started = started || len(rendered.rows) > 0
	}
	if state.inFence {
		if !state.plainFence {
			output.append(renderCodeBlock(markdownPrefix(prefix, started), state, columns, color))
			started = started || len(state.code) > 0
		}
	}
	if !started {
		output.append(layoutRichRuns([]textRun{{text: prefix}}, hangingIndent(prefix), columns))
	}
	return output
}

func renderMarkdownLine(prefix, line string, columns int, state *markdownState, color bool) richRows {
	line = strings.TrimSuffix(line, "\r")
	if state.inFence {
		if isFenceClosing(line, state.fenceChar, state.fenceLength) {
			var rendered richRows
			if !state.plainFence {
				rendered = renderCodeBlock(prefix, *state, columns, color)
			}
			*state = markdownState{}
			return rendered
		}
		if state.plainFence {
			return layoutRichRuns(withMarkdownPrefix(prefix, []textRun{{text: line}}), hangingIndent(prefix), columns)
		}
		appendCodeLine(state, line)
		if state.codeBytes > maxHighlightBytes || len(state.code) > maxHighlightLines {
			rendered := renderCodeBlock(prefix, *state, columns, false)
			state.plainFence = true
			state.code = nil
			state.codeBytes = 0
			return rendered
		}
		return richRows{}
	}

	if marker, ok := parseFenceOpening(line); ok {
		state.inFence = true
		state.fenceChar = marker.char
		state.fenceLength = marker.length
		state.language = marker.language
		state.codeBytes = 0
		state.lexer = lexerForLanguage(marker.language)
		return richRows{}
	}

	runs := markdownLineRuns(line, columns-displayWidth(prefix))
	return layoutRichRuns(withMarkdownPrefix(prefix, runs), hangingIndent(prefix), columns)
}

func appendCodeLine(state *markdownState, line string) {
	if len(state.code) > 0 {
		state.codeBytes++ // newline inserted by strings.Join in highlightCodeBlock
	}
	state.codeBytes += len(line)
	state.code = append(state.code, line)
}

func markdownPrefix(prefix string, started bool) string {
	if started {
		return hangingIndent(prefix)
	}
	return prefix
}

func hangingIndent(prefix string) string {
	return strings.Repeat(" ", displayWidth(prefix))
}

func withMarkdownPrefix(prefix string, content []textRun) []textRun {
	if prefix == "" {
		return content
	}
	if strings.HasPrefix(prefix, "- ") {
		runs := []textRun{{text: "-", style: ansiStyle{color: "32"}}, {text: prefix[1:]}}
		return append(runs, content...)
	}
	return append([]textRun{{text: prefix}}, content...)
}

type fenceMarker struct {
	char     rune
	length   int
	language string
}

func parseFenceOpening(line string) (fenceMarker, bool) {
	trimmed, ok := trimFenceIndent(line)
	if !ok || trimmed == "" {
		return fenceMarker{}, false
	}
	char, _ := utf8.DecodeRuneInString(trimmed)
	if char != '`' && char != '~' {
		return fenceMarker{}, false
	}
	count := 0
	position := 0
	for position < len(trimmed) {
		value, width := utf8.DecodeRuneInString(trimmed[position:])
		if value != char {
			break
		}
		count++
		position += width
	}
	if count < 3 {
		return fenceMarker{}, false
	}
	info := strings.TrimSpace(trimmed[position:])
	if char == '`' && strings.ContainsRune(info, '`') {
		return fenceMarker{}, false
	}
	language := ""
	if fields := strings.Fields(info); len(fields) > 0 {
		language = strings.Trim(fields[0], "{}")
		language = strings.TrimPrefix(language, ".")
	}
	return fenceMarker{char: char, length: count, language: language}, true
}

func isFenceClosing(line string, char rune, minimum int) bool {
	trimmed, ok := trimFenceIndent(line)
	if !ok {
		return false
	}
	count := 0
	position := 0
	for position < len(trimmed) {
		value, width := utf8.DecodeRuneInString(trimmed[position:])
		if value != char {
			break
		}
		count++
		position += width
	}
	return count >= minimum && strings.TrimSpace(trimmed[position:]) == ""
}

func trimFenceIndent(line string) (string, bool) {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 3 {
		return "", false
	}
	return line[spaces:], true
}

func lexerForLanguage(language string) chroma.Lexer {
	language = strings.ToLower(strings.TrimSpace(language))
	aliases := map[string]string{
		"c-sharp": "csharp",
		"c#":      "csharp",
		"golang":  "go",
		"python3": "python",
		"py3":     "python",
		"shell":   "bash",
		"sh":      "bash",
		"zsh":     "bash",
	}
	if alias, ok := aliases[language]; ok {
		language = alias
	}
	if language == "" || language == "text" || language == "plaintext" || language == "plain" {
		return nil
	}
	return lexers.Get(language)
}

func renderCodeBlock(prefix string, state markdownState, columns int, color bool) richRows {
	codeRuns := make([][]textRun, len(state.code))
	for index, line := range state.code {
		codeRuns[index] = []textRun{{text: line}}
	}
	if color && state.lexer != nil && state.codeBytes <= maxHighlightBytes && len(state.code) <= maxHighlightLines {
		if highlighted, ok := highlightCodeBlock(state.lexer, state.code); ok {
			codeRuns = highlighted
		}
	}
	if len(codeRuns) == 0 {
		codeRuns = [][]textRun{{}}
	}
	var output richRows
	for index, runs := range codeRuns {
		linePrefix := prefix
		if index > 0 {
			linePrefix = hangingIndent(prefix)
		}
		output.append(layoutRichRuns(withMarkdownPrefix(linePrefix, runs), hangingIndent(prefix), columns))
	}
	return output
}

func highlightCodeBlock(lexer chroma.Lexer, lines []string) (runs [][]textRun, ok bool) {
	if lexer == nil || len(lines) > maxHighlightLines {
		return nil, false
	}
	source := strings.Join(lines, "\n")
	if len(source) > maxHighlightBytes {
		return nil, false
	}
	defer func() {
		if recover() != nil {
			runs, ok = nil, false
		}
	}()
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, source)
	if err != nil {
		return nil, false
	}
	runs = make([][]textRun, 1)
	remaining := len(source)
	var reconstructed strings.Builder
	for remaining > 0 {
		token := iterator()
		if token == chroma.EOF {
			break
		}
		value := token.Value
		if len(value) > remaining {
			value = value[:remaining]
		}
		remaining -= len(value)
		reconstructed.WriteString(value)
		for {
			newline := strings.IndexByte(value, '\n')
			if newline < 0 {
				appendTextRun(&runs[len(runs)-1], value, codeTokenStyle(token.Type))
				break
			}
			appendTextRun(&runs[len(runs)-1], value[:newline], codeTokenStyle(token.Type))
			runs = append(runs, nil)
			value = value[newline+1:]
		}
	}
	if remaining != 0 || reconstructed.String() != source || len(runs) != len(lines) {
		return nil, false
	}
	return runs, true
}

func codeTokenStyle(token chroma.TokenType) ansiStyle {
	switch {
	case token.InCategory(chroma.Keyword):
		return ansiStyle{color: "35", bold: true}
	case token.InSubCategory(chroma.NameFunction), token.InSubCategory(chroma.NameBuiltin), token == chroma.NameClass, token == chroma.NameNamespace:
		return ansiStyle{color: "36"}
	case token == chroma.NameDecorator, token == chroma.NameConstant, token == chroma.NameAttribute:
		return ansiStyle{color: "33"}
	case token.InSubCategory(chroma.LiteralString):
		return ansiStyle{color: "32"}
	case token.InSubCategory(chroma.LiteralNumber):
		return ansiStyle{color: "33"}
	case token.InCategory(chroma.Comment):
		return ansiStyle{color: "90", dim: true, italic: true}
	case token.InCategory(chroma.Operator):
		return ansiStyle{color: "36"}
	case token == chroma.GenericDeleted, token == chroma.GenericError, token == chroma.Error:
		return ansiStyle{color: "31"}
	case token == chroma.GenericInserted:
		return ansiStyle{color: "32"}
	case token == chroma.GenericHeading, token == chroma.GenericSubheading:
		return ansiStyle{color: "36", bold: true}
	default:
		return ansiStyle{}
	}
}

func markdownLineRuns(line string, available int) []textRun {
	leading := line[:len(line)-len(strings.TrimLeft(line, " "))]
	content := line[len(leading):]
	if level, rest, ok := markdownHeading(content); ok {
		base := ansiStyle{color: "36", bold: true}
		return append([]textRun{{text: leading}}, markdownInlineRuns(rest, base)...)
	} else {
		_ = level
	}
	if marker, rest, ok := markdownBlockquote(content); ok {
		runs := []textRun{{text: leading + marker, style: ansiStyle{color: "90", dim: true}}}
		return append(runs, markdownInlineRuns(rest, ansiStyle{})...)
	}
	if marker, rest, ok := markdownListMarker(content); ok {
		runs := []textRun{{text: leading}, {text: marker, style: ansiStyle{color: "36", bold: true}}}
		return append(runs, markdownInlineRuns(rest, ansiStyle{})...)
	}
	if isThematicBreak(content) {
		width := max(3, available)
		return []textRun{{text: strings.Repeat("─", width), style: ansiStyle{color: "90", dim: true}}}
	}
	return append([]textRun{{text: leading}}, markdownInlineRuns(content, ansiStyle{})...)
}

func markdownHeading(value string) (int, string, bool) {
	level := 0
	for level < len(value) && level < 6 && value[level] == '#' {
		level++
	}
	if level == 0 || level >= len(value) || value[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(value[level:]), true
}

func markdownBlockquote(value string) (string, string, bool) {
	if !strings.HasPrefix(value, ">") {
		return "", "", false
	}
	rest := strings.TrimPrefix(value[1:], " ")
	return "│ ", rest, true
}

func markdownListMarker(value string) (string, string, bool) {
	if len(value) >= 2 && strings.ContainsRune("-*+", rune(value[0])) && value[1] == ' ' {
		marker, rest := "• ", value[2:]
		if checkbox, tail, ok := markdownCheckbox(rest); ok {
			return marker + checkbox, tail, true
		}
		return marker, rest, true
	}
	position := 0
	for position < len(value) && value[position] >= '0' && value[position] <= '9' {
		position++
	}
	if position > 0 && position+1 < len(value) && (value[position] == '.' || value[position] == ')') && value[position+1] == ' ' {
		return value[:position+2], value[position+2:], true
	}
	return "", "", false
}

func markdownCheckbox(value string) (string, string, bool) {
	if len(value) < 4 || value[0] != '[' || value[2] != ']' || value[3] != ' ' {
		return "", "", false
	}
	switch value[1] {
	case ' ', 'x', 'X':
		box := "☐ "
		if value[1] != ' ' {
			box = "☑ "
		}
		return box, value[4:], true
	default:
		return "", "", false
	}
}

func isThematicBreak(value string) bool {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
	if len(compact) < 3 {
		return false
	}
	return strings.Trim(compact, string(compact[0])) == "" && strings.ContainsRune("-*_", rune(compact[0]))
}

func markdownInlineRuns(value string, base ansiStyle) []textRun {
	var runs []textRun
	for position := 0; position < len(value); {
		if value[position] == '\\' && position+1 < len(value) {
			next, size := utf8.DecodeRuneInString(value[position+1:])
			if isMarkdownEscapable(next) {
				appendTextRun(&runs, value[position+1:position+1+size], base)
				position += 1 + size
				continue
			}
			appendTextRun(&runs, "\\", base)
			position++
			continue
		}
		remaining := value[position:]
		if (strings.HasPrefix(remaining, "**") || strings.HasPrefix(remaining, "__")) && inlineDelimiterCanOpen(value, position, 2) {
			marker := remaining[:2]
			if end := findInlineDelimiterClose(value, position+2, marker); end >= 0 {
				style := mergeANSIStyle(base, ansiStyle{bold: true})
				appendTextRun(&runs, value[position+2:end], style)
				position = end + 2
				continue
			}
			appendTextRun(&runs, remaining, base)
			break
		}
		if strings.HasPrefix(remaining, "~~") && inlineDelimiterCanOpen(value, position, 2) {
			if end := findInlineDelimiterClose(value, position+2, "~~"); end >= 0 {
				style := mergeANSIStyle(base, ansiStyle{strike: true})
				appendTextRun(&runs, value[position+2:end], style)
				position = end + 2
				continue
			}
			appendTextRun(&runs, remaining, base)
			break
		}
		if value[position] == '`' {
			count := strings.IndexFunc(remaining, func(r rune) bool { return r != '`' })
			if count < 0 {
				appendTextRun(&runs, remaining, base)
				break
			}
			marker := strings.Repeat("`", count)
			if relativeEnd := strings.Index(remaining[count:], marker); relativeEnd >= 0 {
				end := position + count + relativeEnd
				code := value[position+count : end]
				if strings.HasPrefix(code, " ") && strings.HasSuffix(code, " ") && strings.TrimSpace(code) != "" {
					code = code[1 : len(code)-1]
				}
				appendTextRun(&runs, code, mergeANSIStyle(base, ansiStyle{color: "33"}))
				position = end + count
				continue
			}
			appendTextRun(&runs, remaining, base)
			break
		}
		if value[position] == '[' {
			if relativeLabel := strings.Index(remaining, "]("); relativeLabel > 0 {
				closeLabel := position + relativeLabel
				if relativeURL := strings.IndexByte(value[closeLabel+2:], ')'); relativeURL >= 0 {
					closeURL := closeLabel + 2 + relativeURL
					label := value[position+1 : closeLabel]
					url := value[closeLabel+2 : closeURL]
					appendTextRun(&runs, label, mergeANSIStyle(base, ansiStyle{underline: true}))
					appendTextRun(&runs, " ("+url+")", mergeANSIStyle(base, ansiStyle{color: "36", underline: true}))
					position = closeURL + 1
					continue
				}
			}
			appendTextRun(&runs, remaining, base)
			break
		}
		if (value[position] == '*' || value[position] == '_') && inlineDelimiterCanOpen(value, position, 1) {
			marker := value[position : position+1]
			if end := findInlineDelimiterClose(value, position+1, marker); end >= 0 {
				style := mergeANSIStyle(base, ansiStyle{italic: true})
				appendTextRun(&runs, value[position+1:end], style)
				position = end + 1
				continue
			}
			appendTextRun(&runs, remaining, base)
			break
		}
		_, size := utf8.DecodeRuneInString(remaining)
		appendTextRun(&runs, remaining[:size], base)
		position += size
	}
	return runs
}

func isMarkdownEscapable(value rune) bool {
	return value >= '!' && value <= '~' && (unicode.IsPunct(value) || unicode.IsSymbol(value))
}

func inlineDelimiterCanOpen(value string, position, size int) bool {
	after, ok := runeAfter(value, position+size)
	if !ok || unicode.IsSpace(after) {
		return false
	}
	before, hasBefore := runeBefore(value, position)
	if hasBefore && isWordRune(before) && isWordRune(after) {
		return false
	}
	return true
}

func inlineDelimiterCanClose(value string, position, size int) bool {
	before, ok := runeBefore(value, position)
	if !ok || unicode.IsSpace(before) {
		return false
	}
	after, hasAfter := runeAfter(value, position+size)
	if hasAfter && isWordRune(before) && isWordRune(after) {
		return false
	}
	return true
}

func findInlineDelimiterClose(value string, start int, marker string) int {
	for start < len(value) {
		relative := strings.Index(value[start:], marker)
		if relative < 0 {
			return -1
		}
		position := start + relative
		if !markdownDelimiterEscaped(value, position) && inlineDelimiterCanClose(value, position, len(marker)) {
			return position
		}
		start = position + len(marker)
	}
	return -1
}

func markdownDelimiterEscaped(value string, position int) bool {
	backslashes := 0
	for position > 0 && value[position-1] == '\\' {
		backslashes++
		position--
	}
	return backslashes%2 != 0
}

func runeBefore(value string, position int) (rune, bool) {
	if position <= 0 || position > len(value) {
		return 0, false
	}
	result, _ := utf8.DecodeLastRuneInString(value[:position])
	return result, true
}

func runeAfter(value string, position int) (rune, bool) {
	if position < 0 || position >= len(value) {
		return 0, false
	}
	result, _ := utf8.DecodeRuneInString(value[position:])
	return result, true
}

func isWordRune(value rune) bool {
	return unicode.IsLetter(value) || unicode.IsDigit(value)
}

func mergeANSIStyle(base, overlay ansiStyle) ansiStyle {
	if overlay.color != "" {
		base.color = overlay.color
	}
	base.bold = base.bold || overlay.bold
	base.dim = base.dim || overlay.dim
	base.italic = base.italic || overlay.italic
	base.underline = base.underline || overlay.underline
	base.strike = base.strike || overlay.strike
	return base
}

func appendTextRun(runs *[]textRun, text string, style ansiStyle) {
	if text == "" {
		return
	}
	if len(*runs) > 0 && (*runs)[len(*runs)-1].style == style {
		(*runs)[len(*runs)-1].text += text
		return
	}
	*runs = append(*runs, textRun{text: text, style: style})
}

func appendRuns(target *[]textRun, values []textRun) {
	for _, value := range values {
		appendTextRun(target, value.text, value.style)
	}
}

func layoutRichRuns(runs []textRun, indent string, columns int) richRows {
	if columns <= 0 {
		columns = defaultColumns
	}
	indentWidth := displayWidth(indent)
	if indentWidth >= columns {
		indent, indentWidth = "", 0
	}
	output := richRows{rows: []string{""}, spans: [][]textSpan{nil}}
	row, column := 0, 0
	appendRune := func(value rune, style ansiStyle) {
		start := len(output.rows[row])
		output.rows[row] += string(value)
		end := len(output.rows[row])
		if style == (ansiStyle{}) {
			return
		}
		spans := output.spans[row]
		if len(spans) > 0 && spans[len(spans)-1].end == start && spans[len(spans)-1].style == style {
			spans[len(spans)-1].end = end
			output.spans[row] = spans
			return
		}
		output.spans[row] = append(spans, textSpan{start: start, end: end, style: style})
	}
	newRow := func() {
		output.rows = append(output.rows, indent)
		output.spans = append(output.spans, nil)
		row++
		column = indentWidth
	}
	for _, run := range runs {
		for _, raw := range run.text {
			if raw == '\n' {
				newRow()
				continue
			}
			for _, value := range expandRune(raw) {
				width := runeWidth(value)
				if column > 0 && column+width > columns {
					newRow()
				}
				appendRune(value, run.style)
				column += width
			}
		}
	}
	return output
}

func (r *richRows) append(other richRows) {
	r.rows = append(r.rows, other.rows...)
	r.spans = append(r.spans, other.spans...)
}

func (r richRows) tail(limit int) richRows {
	if limit < 0 {
		limit = 0
	}
	if len(r.rows) <= limit {
		return r
	}
	start := len(r.rows) - limit
	return richRows{rows: r.rows[start:], spans: r.spans[start:]}
}
