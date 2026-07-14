package terminal

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	defaultLiveRows = 6
	defaultColumns  = 80
)

// RendererConfig configures a LiveRenderer.
type RendererConfig struct {
	TTY         bool
	Color       bool
	Columns     int
	MaxRows     int
	ColumnsFunc func() int
}

// PromptState describes an editable prompt. Cursor is a rune index in Text.
type PromptState struct {
	Prefix      string
	Text        string
	Cursor      int
	Completions []Candidate
	Selected    int
}

// LiveFrame combines an active response with the always-visible input area.
// Pending entries are displayed inside the input area below its top divider.
type LiveFrame struct {
	MessagePrefix string
	Message       string
	Context       []string
	Activity      []string
	Status        string
	InputLeft     string
	InputRight    string
	Pending       []string
	Prompt        PromptState
	Busy          bool
	Spinner       string
	ShowDivider   bool
}

// LiveRenderer owns the terminal's bounded, redrawable bottom region. No
// other type in this package writes terminal control sequences.
type LiveRenderer struct {
	mu        sync.Mutex
	w         io.Writer
	tty       bool
	color     bool
	columns   int
	maxRows   int
	columnsFn func() int
	rows      []string
	cursorRow int
	cursorCol int
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
		color:     config.Color && config.TTY,
		columns:   columns,
		maxRows:   maxRows,
		columnsFn: config.ColumnsFunc,
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
	r.syncColumns()
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
	r.syncColumns()

	prefix := Sanitize(state.Prefix)
	cleanText := Sanitize(state.Text)
	text := prefix + cleanText
	cursor := runeCount(prefix) + clamp(state.Cursor, 0, runeCount(cleanText))
	rows, cursorRow, cursorCol := layoutTextHanging(text, cursor, r.columns, strings.Repeat(" ", displayWidth(prefix)))
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

// Frame redraws a composite response, pending queue, and editor while keeping
// the cursor in the editor. The input divider and prompt always have priority
// over transient response rows when the bounded region is full.
func (r *LiveRenderer) Frame(frame LiveFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}
	r.syncColumns()
	prompt := frame.Prompt
	if frame.Busy {
		spinner := frame.Spinner
		if spinner == "" {
			spinner = "*"
		}
		prompt.Prefix = spinner + " " + prompt.Prefix
	}
	promptRows, promptCursorRow, promptCursorCol := r.promptRows(prompt, max(1, r.maxRows-1))

	dividerRows := 0
	if frame.ShowDivider {
		dividerRows = 1
	}
	barRows := 0
	if !frame.ShowDivider && (frame.InputLeft != "" || frame.InputRight != "") {
		barRows = 1
	}
	available := max(0, r.maxRows-dividerRows-barRows-len(promptRows))
	pendingCount := min(len(frame.Pending), min(2, available))
	var pendingRows []string
	for i := len(frame.Pending) - 1; i >= len(frame.Pending)-pendingCount; i-- {
		item := frame.Pending[i]
		line, _, _ := strings.Cut(strings.TrimSpace(Sanitize(item)), "\n")
		group := []string{queuedPreview(line, r.columns)}
		if len(pendingRows) >= available {
			continue
		}
		pendingRows = append(group, pendingRows...)
	}

	inputRows := make([]string, 0, dividerRows+barRows+len(pendingRows)+len(promptRows))
	if frame.ShowDivider {
		inputRows = append(inputRows, dividerStatusBar(frame.InputLeft, frame.InputRight, r.columns))
	}
	if barRows > 0 {
		inputRows = append(inputRows, statusBar(frame.InputLeft, frame.InputRight, r.columns))
	}
	inputRows = append(inputRows, pendingRows...)
	inputRows = append(inputRows, promptRows...)
	remaining := max(0, r.maxRows-len(inputRows))

	contextRows := make([]string, 0, len(frame.Context))
	for _, item := range frame.Context {
		contextRows = append(contextRows, r.layoutLines([]string{item})...)
	}
	if len(contextRows) > remaining {
		contextRows = contextRows[:remaining]
	}
	remaining -= len(contextRows)

	var activity []string
	for _, item := range frame.Activity {
		activity = append(activity, r.layoutLines([]string{item})...)
	}
	if frame.Status != "" {
		activity = append(activity, r.layoutLines([]string{"status: " + frame.Status})...)
	}
	if frame.Message != "" || frame.MessagePrefix != "" {
		activity = append(activity, r.messageRows(frame.MessagePrefix, frame.Message)...)
	}
	if len(activity) > remaining {
		activity = activity[len(activity)-remaining:]
	}
	rows := append(contextRows, activity...)
	rows = append(rows, inputRows...)
	cursorRow := len(contextRows) + len(activity) + dividerRows + barRows + len(pendingRows) + promptCursorRow
	if !r.tty {
		return r.writePlain(rows)
	}
	return r.redraw(rows, cursorRow, promptCursorCol)
}

func (r *LiveRenderer) promptRows(state PromptState, limit int) ([]string, int, int) {
	prefix := Sanitize(state.Prefix)
	cleanText := Sanitize(state.Text)
	text := prefix + cleanText
	cursor := runeCount(prefix) + clamp(state.Cursor, 0, runeCount(cleanText))
	rows, cursorRow, cursorCol := layoutTextHanging(text, cursor, r.columns, strings.Repeat(" ", displayWidth(prefix)))
	if len(rows) >= limit {
		start := cursorRow - limit + 1
		if start < 0 {
			start = 0
		}
		if start+limit > len(rows) {
			start = len(rows) - limit
		}
		rows = rows[start : start+limit]
		cursorRow = clamp(cursorRow-start, 0, len(rows)-1)
		return rows, cursorRow, cursorCol
	}
	choiceBudget := limit - len(rows)
	start := 0
	if state.Selected >= choiceBudget {
		start = state.Selected - choiceBudget + 1
	}
	end := min(len(state.Completions), start+choiceBudget)
	for i := start; i < end; i++ {
		candidate := state.Completions[i]
		marker := "  "
		if i == state.Selected {
			marker = "> "
		}
		line := marker + Sanitize(candidate.Value)
		if candidate.Description != "" {
			line += "  " + Sanitize(candidate.Description)
		}
		group := wrapLine(line, r.columns)
		if i < state.Selected && len(rows)+len(group) >= limit {
			continue
		}
		if len(rows)+len(group) > limit {
			if i == state.Selected && len(rows) < limit {
				rows = append(rows, group[:limit-len(rows)]...)
			}
			continue
		}
		rows = append(rows, group...)
	}
	return rows, cursorRow, cursorCol
}

func statusBar(left, right string, columns int) string {
	left = Sanitize(left)
	right = Sanitize(right)
	if columns <= 1 {
		return ""
	}
	width := columns - 1
	if right == "" {
		return truncateWidth(left, width)
	}
	if left == "" {
		return truncateWidth(right, width)
	}
	if displayWidth(left)+1+displayWidth(right) <= width {
		return left + strings.Repeat(" ", width-displayWidth(left)-displayWidth(right)) + right
	}
	rightWidth := min(displayWidth(right), max(1, width/2))
	right = truncateWidth(right, rightWidth)
	leftWidth := max(0, width-displayWidth(right)-1)
	left = truncateWidth(left, leftWidth)
	if left == "" {
		return truncateWidth(right, width)
	}
	return left + " " + right
}

func dividerStatusBar(left, right string, columns int) string {
	width := max(3, columns-1)
	left = Sanitize(left)
	right = Sanitize(right)
	if left == "" && right == "" {
		return strings.Repeat("─", width)
	}
	if right == "" {
		value := truncateWidth(left, max(1, width-2))
		return "─" + value + strings.Repeat("─", max(1, width-displayWidth(value)-1))
	}
	if left == "" {
		value := truncateWidth(right, max(1, width-2))
		return strings.Repeat("─", max(1, width-displayWidth(value)-1)) + value + "─"
	}
	leftWidth := max(1, (width-3)/2)
	rightWidth := max(1, width-displayWidth(truncateWidth(left, leftWidth))-3)
	left = truncateWidth(left, leftWidth)
	right = truncateWidth(right, rightWidth)
	middle := max(1, width-displayWidth(left)-displayWidth(right)-2)
	return "─" + left + strings.Repeat("─", middle) + right + "─"
}

func queuedPreview(value string, columns int) string {
	const prefix = "$ "
	const suffix = "  (queued)"
	available := columns - displayWidth(prefix) - displayWidth(suffix)
	if available <= 0 {
		return truncateWidth(prefix+"(queued)", max(1, columns-1))
	}
	if displayWidth(value) > available {
		value = truncateWidth(value, max(1, available-1)) + "…"
	}
	return prefix + value + suffix
}

func truncateWidth(value string, width int) string {
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

// UpdateMessage replaces the live region with a hanging-indented message.
func (r *LiveRenderer) UpdateMessage(prefix, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}
	r.syncColumns()
	rows := r.messageRows(prefix, text)
	if len(rows) > r.maxRows {
		rows = rows[len(rows)-r.maxRows:]
	}
	if !r.tty {
		return r.writePlain(rows)
	}
	row, col := lastPosition(rows)
	return r.redraw(rows, row, col)
}

// CommitMessage appends a permanent hanging-indented message. When divider is
// true, a dim rule is committed beneath it before the live response begins.
func (r *LiveRenderer) CommitMessage(prefix, text string, divider bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}
	r.syncColumns()
	rows := r.messageRows(prefix, text)
	if divider {
		rows = append(rows, strings.Repeat("─", max(3, r.columns-1)))
	}
	return r.commitRows(rows)
}

// CommitDivider appends one permanent input-boundary rule.
func (r *LiveRenderer) CommitDivider() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("terminal: renderer is closed")
	}
	r.syncColumns()
	return r.commitRows([]string{strings.Repeat("─", max(3, r.columns-1))})
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
	r.syncColumns()
	clean := Sanitize(text)
	var output strings.Builder
	if r.tty && len(r.rows) > 0 {
		r.buildRedraw(&output, nil, 0, 0)
	}
	parts := strings.Split(clean, "\n")
	for i, part := range parts {
		if i > 0 {
			output.WriteByte('\n')
		}
		output.WriteString(r.decorate(part))
	}
	if !strings.HasSuffix(clean, "\n") {
		output.WriteByte('\n')
	}
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = nil
	r.cursorRow = 0
	r.cursorCol = 0
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
	r.cursorCol = cursorCol
	return nil
}

func (r *LiveRenderer) buildRedraw(output *strings.Builder, rows []string, cursorRow, cursorCol int) {
	// Cursor visibility changes surround one buffered write, not persistent UI.
	output.WriteString("\x1b[?25l")
	if len(r.rows) > 0 {
		output.WriteByte('\r')
		moveUp(output, physicalCursorRow(r.rows, r.cursorRow, r.cursorCol, r.columns))
	}
	count := max(physicalRowCount(r.rows, r.columns), len(rows))
	for i := 0; i < count; i++ {
		output.WriteString("\x1b[2K")
		if i < len(rows) {
			output.WriteString(r.decorate(rows[i]))
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
			output.WriteString(r.decorate(prefixWidth(rows[cursorRow], cursorCol)))
		}
	}
	output.WriteString("\x1b[?25h")
}

func (r *LiveRenderer) commitRows(rows []string) error {
	var output strings.Builder
	if r.tty && len(r.rows) > 0 {
		r.buildRedraw(&output, nil, 0, 0)
	}
	for _, row := range rows {
		output.WriteString(r.decorate(row))
		output.WriteByte('\n')
	}
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = nil
	r.cursorRow = 0
	r.cursorCol = 0
	return nil
}

func (r *LiveRenderer) messageRows(prefix, text string) []string {
	prefix = Sanitize(prefix)
	text = prefix + strings.TrimRight(Sanitize(text), "\r\n")
	rows, _, _ := layoutTextHanging(text, runeCount(text), r.columns, strings.Repeat(" ", displayWidth(prefix)))
	return rows
}

func (r *LiveRenderer) syncColumns() {
	if r.columnsFn == nil {
		return
	}
	if columns := r.columnsFn(); columns > 0 {
		r.columns = columns
	}
}

func (r *LiveRenderer) decorate(row string) string {
	if !r.color || row == "" {
		return row
	}
	color := func(code, value string) string { return "\x1b[" + code + "m" + value + "\x1b[0m" }
	switch {
	case hasSpinnerPrefix(row):
		spinner, rest := firstRune(row)
		if strings.HasPrefix(rest, " $ ") {
			return color("36", spinner) + " " + color("36", "$") + rest[2:]
		}
		return color("36", spinner) + rest
	case strings.HasPrefix(row, "$ "):
		return color("36", "$") + row[1:]
	case strings.HasPrefix(row, "- "):
		return color("32", "-") + row[1:]
	case strings.Trim(row, "─") == "":
		return color("2;90", row)
	case strings.HasPrefix(row, "error:"):
		return color("31", row)
	case strings.HasPrefix(row, "status:") || strings.HasPrefix(row, "tool:"):
		return color("2", row)
	case strings.HasPrefix(row, "> "):
		return color("36", ">") + row[1:]
	default:
		return row
	}
}

func hasSpinnerPrefix(value string) bool {
	if value == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(value)
	return strings.ContainsRune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏", r)
}

func firstRune(value string) (string, string) {
	_, size := utf8.DecodeRuneInString(value)
	return value[:size], value[size:]
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
	return layoutTextHanging(text, cursor, columns, "")
}

func layoutTextHanging(text string, cursor, columns int, indent string) ([]string, int, int) {
	rows := []string{""}
	row, col, seen := 0, 0, 0
	cursorRow, cursorCol := 0, 0
	indentWidth := displayWidth(indent)
	if indentWidth >= columns {
		indent, indentWidth = "", 0
	}
	for _, rawRune := range text {
		if seen == cursor {
			cursorRow, cursorCol = row, col
		}
		seen++
		if rawRune == '\n' {
			rows = append(rows, indent)
			row++
			col = indentWidth
			continue
		}
		for _, value := range expandRune(rawRune) {
			width := runeWidth(value)
			if col > 0 && col+width > columns {
				rows = append(rows, indent)
				row++
				col = indentWidth
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

func physicalRowCount(rows []string, columns int) int {
	count := 0
	for _, row := range rows {
		count += len(wrapLine(row, columns))
	}
	return count
}

func physicalCursorRow(rows []string, cursorRow, cursorCol, columns int) int {
	if len(rows) == 0 {
		return 0
	}
	cursorRow = clamp(cursorRow, 0, len(rows)-1)
	physical := 0
	for i := 0; i < cursorRow; i++ {
		physical += len(wrapLine(rows[i], columns))
	}
	prefix := prefixWidth(rows[cursorRow], cursorCol)
	return physical + len(wrapLine(prefix, columns)) - 1
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
