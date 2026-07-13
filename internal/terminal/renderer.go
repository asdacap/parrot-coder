package terminal

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
)

const (
	defaultLiveRows = 6
	defaultColumns  = 80
)

// RendererConfig configures a LiveRenderer.
type RendererConfig struct {
	TTY     bool
	Columns int
	MaxRows int
}

// PromptState describes an editable prompt. Cursor is a rune index in Text.
type PromptState struct {
	Prefix      string
	Text        string
	Cursor      int
	Completions []Candidate
	Selected    int
}

// LiveRenderer owns the terminal's bounded, redrawable bottom region. No
// other type in this package writes terminal control sequences.
type LiveRenderer struct {
	mu        sync.Mutex
	w         io.Writer
	tty       bool
	columns   int
	maxRows   int
	rows      []string
	cursorRow int
	plainSeen map[string]struct{}
	closed    bool
}

// NewLiveRenderer creates a renderer. TTY must only be enabled for a terminal
// writer; plain mode never emits escape sequences.
func NewLiveRenderer(w io.Writer, config RendererConfig) *LiveRenderer {
	columns := config.Columns
	if columns <= 0 {
		columns = defaultColumns
	}
	maxRows := config.MaxRows
	if maxRows <= 0 {
		maxRows = defaultLiveRows
	}
	return &LiveRenderer{
		w:         w,
		tty:       config.TTY,
		columns:   columns,
		maxRows:   maxRows,
		plainSeen: make(map[string]struct{}),
	}
}

// SetColumns updates the wrapping width used by subsequent redraws.
func (r *LiveRenderer) SetColumns(columns int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if columns > 0 {
		r.columns = columns
	}
}

// Update replaces the live region. Input is sanitized and physically wrapped.
func (r *LiveRenderer) Update(lines []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}
	rows := r.layoutLines(lines)
	if !r.tty {
		return r.writePlain(rows)
	}
	row, col := lastPosition(rows)
	return r.redraw(rows, row, col)
}

// Prompt redraws an editable prompt and any completion choices.
func (r *LiveRenderer) Prompt(state PromptState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}

	text := Sanitize(state.Prefix) + Sanitize(state.Text)
	cursor := runeCount(Sanitize(state.Prefix)) + clamp(state.Cursor, 0, runeCount(Sanitize(state.Text)))
	rows, cursorRow, cursorCol := layoutText(text, cursor, r.columns)
	for i, candidate := range state.Completions {
		if i >= r.maxRows {
			break
		}
		marker := "  "
		if i == state.Selected {
			marker = "> "
		}
		line := marker + Sanitize(candidate.Value)
		if candidate.Description != "" {
			line += "  " + Sanitize(candidate.Description)
		}
		rows = append(rows, wrapLine(line, r.columns)...)
	}
	if len(rows) > r.maxRows {
		start := cursorRow - r.maxRows + 1
		if start < 0 {
			start = 0
		}
		if start+r.maxRows > len(rows) {
			start = len(rows) - r.maxRows
		}
		rows = rows[start : start+r.maxRows]
		cursorRow -= start
		cursorRow = clamp(cursorRow, 0, len(rows)-1)
	}
	if !r.tty {
		return r.writePlain(rows)
	}
	return r.redraw(rows, cursorRow, cursorCol)
}

// Clear erases only the renderer-owned live region.
func (r *LiveRenderer) Clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.tty || len(r.rows) == 0 {
		return nil
	}
	return r.redraw(nil, 0, 0)
}

// Commit clears the live region and appends permanent sanitized text.
func (r *LiveRenderer) Commit(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}
	clean := Sanitize(text)
	var output strings.Builder
	if r.tty && len(r.rows) > 0 {
		r.buildRedraw(&output, nil, 0, 0)
	}
	output.WriteString(clean)
	if !strings.HasSuffix(clean, "\n") {
		output.WriteByte('\n')
	}
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = nil
	r.cursorRow = 0
	return nil
}

// Close clears the live region. It is idempotent.
func (r *LiveRenderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	var err error
	if r.tty && len(r.rows) > 0 {
		var output strings.Builder
		r.buildRedraw(&output, nil, 0, 0)
		err = writeAtomic(r.w, output.String())
		if err == nil {
			r.rows = nil
		}
	}
	r.closed = true
	return err
}

func (r *LiveRenderer) redraw(rows []string, cursorRow, cursorCol int) error {
	var output strings.Builder
	r.buildRedraw(&output, rows, cursorRow, cursorCol)
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = append(r.rows[:0], rows...)
	r.cursorRow = cursorRow
	return nil
}

func (r *LiveRenderer) buildRedraw(output *strings.Builder, rows []string, cursorRow, cursorCol int) {
	// Cursor visibility changes surround one buffered write, not persistent UI.
	output.WriteString("\x1b[?25l")
	if len(r.rows) > 0 {
		output.WriteByte('\r')
		moveUp(output, r.cursorRow)
	}
	count := max(len(r.rows), len(rows))
	for i := 0; i < count; i++ {
		output.WriteString("\x1b[2K")
		if i < len(rows) {
			output.WriteString(rows[i])
		}
		if i+1 < count {
			output.WriteString("\r\n")
		}
	}
	if count > 0 {
		if len(rows) == 0 {
			output.WriteByte('\r')
			moveUp(output, count-1)
		} else if cursorRow != count-1 || cursorCol != displayWidth(rows[cursorRow]) {
			output.WriteByte('\r')
			moveUp(output, count-1-cursorRow)
			output.WriteString(prefixWidth(rows[cursorRow], cursorCol))
		}
	}
	output.WriteString("\x1b[?25h")
}

func (r *LiveRenderer) writePlain(rows []string) error {
	var output strings.Builder
	added := make([]string, 0, len(rows))
	pending := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if _, exists := r.plainSeen[row]; exists {
			continue
		}
		if _, exists := pending[row]; exists {
			continue
		}
		pending[row] = struct{}{}
		added = append(added, row)
		output.WriteString(row)
		output.WriteByte('\n')
	}
	if output.Len() == 0 {
		return nil
	}
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	for _, row := range added {
		r.plainSeen[row] = struct{}{}
	}
	return nil
}

func (r *LiveRenderer) layoutLines(lines []string) []string {
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		for _, part := range strings.Split(Sanitize(line), "\n") {
			rows = append(rows, wrapLine(part, r.columns)...)
		}
	}
	if len(rows) > r.maxRows {
		rows = rows[len(rows)-r.maxRows:]
	}
	return rows
}

func layoutText(text string, cursor, columns int) ([]string, int, int) {
	rows := []string{""}
	row, col, seen := 0, 0, 0
	cursorRow, cursorCol := 0, 0
	for _, rawRune := range text {
		if seen == cursor {
			cursorRow, cursorCol = row, col
		}
		seen++
		if rawRune == '\n' {
			rows = append(rows, "")
			row++
			col = 0
			continue
		}
		for _, value := range expandRune(rawRune) {
			width := runeWidth(value)
			if col > 0 && col+width > columns {
				rows = append(rows, "")
				row++
				col = 0
			}
			rows[row] += string(value)
			col += width
		}
	}
	if seen == cursor {
		cursorRow, cursorCol = row, col
	}
	return rows, cursorRow, cursorCol
}

func wrapLine(line string, columns int) []string {
	rows, _, _ := layoutText(line, runeCount(line), columns)
	return rows
}

func prefixWidth(value string, width int) string {
	var output strings.Builder
	used := 0
	for _, r := range value {
		next := runeWidth(r)
		if used+next > width {
			break
		}
		output.WriteRune(r)
		used += next
	}
	return output.String()
}

func expandRune(r rune) []rune {
	if r == '\t' {
		return []rune("    ")
	}
	return []rune{r}
}

func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	// A conservative approximation avoids under-counting common wide glyphs.
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) || (r >= 0x1f300 && r <= 0x1faff) ||
		(r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

func lastPosition(rows []string) (int, int) {
	if len(rows) == 0 {
		return 0, 0
	}
	return len(rows) - 1, displayWidth(rows[len(rows)-1])
}

func displayWidth(value string) int {
	width := 0
	for _, r := range value {
		width += runeWidth(r)
	}
	return width
}

func moveUp(output *strings.Builder, rows int) {
	if rows > 0 {
		fmt.Fprintf(output, "\x1b[%dA", rows)
	}
}

func runeCount(value string) int {
	return len([]rune(value))
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func writeAtomic(w io.Writer, value string) error {
	if value == "" {
		return nil
	}
	n, err := io.WriteString(w, value)
	if err == nil && n != len(value) {
		return io.ErrShortWrite
	}
	return err
}
