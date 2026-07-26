package terminal

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultLiveRows is the live region's row budget when a renderer is created
	// without an explicit MaxRows.
	DefaultLiveRows         = 10
	defaultInputRows        = 12
	defaultColumns          = 80
	liveBackgroundANSI      = "48;5;236"
	liveForegroundANSI      = "38;5;252"
	liveMutedForegroundANSI = "38;5;245"
)

var (
	errRendererClosed        = errors.New("terminal: renderer is closed")
	errStreamMessageIDEmpty  = errors.New("terminal: stream message ID is empty")
	errStreamMessageConflict = errors.New("terminal: another assistant message is still streaming")
	errStreamPrefixChanged   = errors.New("terminal: stream message prefix changed")
	errStreamTextChanged     = errors.New("terminal: streamed assistant text changed")
	errFrameTaskIDEmpty      = errors.New("terminal: live frame task ID is empty")
	errFrameTaskCycle        = errors.New("terminal: live frame task hierarchy contains a cycle")
	errFrameStatusConflict   = errors.New("terminal: task has multiple main status frames")
)

// RenderErrorClass returns a content-free classification for renderer-owned
// invariant errors. Writer errors are deliberately left unclassified because
// their concrete type is already recorded by the CLI diagnostics layer.
func RenderErrorClass(err error) string {
	switch {
	case errors.Is(err, errRendererClosed):
		return "renderer_closed"
	case errors.Is(err, errStreamMessageIDEmpty):
		return "stream_message_id_empty"
	case errors.Is(err, errStreamMessageConflict):
		return "stream_message_conflict"
	case errors.Is(err, errStreamPrefixChanged):
		return "stream_prefix_changed"
	case errors.Is(err, errStreamTextChanged):
		return "stream_text_changed"
	case errors.Is(err, errFrameTaskIDEmpty):
		return "frame_task_id_empty"
	case errors.Is(err, errFrameTaskCycle):
		return "frame_task_cycle"
	case errors.Is(err, errFrameStatusConflict):
		return "frame_status_conflict"
	default:
		return ""
	}
}

// RendererConfig configures a LiveRenderer.
type RendererConfig struct {
	TTY        bool
	Color      bool
	Columns    int
	InlineDiff bool
	// MaxRows bounds transient status and response rows. It does not include
	// the independently bounded input/menu region.
	MaxRows int
	// MaxInputRows bounds the prompt and an open picker/completion menu.
	MaxInputRows int
	ColumnsFunc  func() int
}

// PromptState describes an editable prompt. Cursor is a rune index in Text.
type PromptState struct {
	Prefix      string
	Text        string
	Cursor      int
	Completions []Candidate
	Selected    int
	// MaxRows optionally lowers the renderer's input-region row limit.
	MaxRows int
}

// TextStyle selects renderer-owned presentation for sanitized terminal text.
// Callers provide plain text only; the renderer applies styling after layout.
type TextStyle uint8

const (
	TextStyleDefault TextStyle = iota
	// TextStyleMuted renders an informational row in ANSI bright black (grey).
	TextStyleMuted
	textStyleGreen
	textStyleUserMessage
	textStyleAssistantMessage
)

type surfaceStyle uint8

const (
	surfaceDefault surfaceStyle = iota
	surfaceLive
)

// StyledText is terminal text with semantic renderer-owned presentation.
// Markdown enables the same safe Markdown subset used for assistant messages;
// Prefix and Suffix are renderer-owned plain-text decorations around it.
type StyledText struct {
	Text     string
	Style    TextStyle
	Markdown bool
	Prefix   string
	Suffix   string
}

// MutedText marks a report for muted grey presentation when color is enabled.
func MutedText(text string) StyledText {
	return StyledText{Text: text, Style: TextStyleMuted}
}

// LiveFrame describes one task-scoped portion of the redrawable region.
// SessionID and ParentSessionID form a flat session tree when passed to Frames;
// TaskID remains the frame grouping key.
// MainStatus marks the frame that contains the task's primary status; child
// tasks are rendered before that frame. Frame accepts the zero-value metadata
// for compatibility with callers rendering one isolated composite frame.
type LiveFrame struct {
	TaskID          string
	SessionID       string
	ParentSessionID string
	MainStatus      bool
	MessagePrefix   string
	Message         string
	Context         []string
	// PromptContext is rendered in full immediately before Prompt. Unlike
	// Context, it is not part of the bounded upper live arena. This is for
	// decision-critical context that must remain visible beside its selector.
	PromptContext  []string
	Activity       []string
	StyledActivity []StyledText
	InputLeft      string
	// InputCenter is transient modeline content between the persistent left and
	// right labels. It is presented as-is rather than as a generic status.
	InputCenter string
	InputRight  string
	Pending     []string
	Prompt      PromptState
	Busy        bool
	Spinner     string
	ShowDivider bool
	Stream      *StreamMessage
}

// StreamMessage is the cumulative text of one in-progress assistant message.
// Complete source lines are moved into normal terminal scrollback while the
// unfinished source line remains in the renderer-owned live region. A fenced
// code block remains live until its closing fence so highlighting can preserve
// multiline lexer state.
type StreamMessage struct {
	ID     string
	Prefix string
	Text   string
}

// commitKind describes how a permanent transcript entry is spaced. Compact
// entries stack without gaps; ordinary block boundaries have an empty row.
type commitKind uint8

const (
	commitCompact commitKind = iota
	commitBlock
)

type liveStream struct {
	id       string
	prefix   string
	text     string
	pending  []rune
	started  bool
	markdown markdownState
}

// LiveRenderer owns the terminal's bounded, redrawable bottom region. No
// other type in this package writes terminal control sequences.
type LiveRenderer struct {
	mu           sync.Mutex
	w            io.Writer
	tty          bool
	color        bool
	columns      int
	maxRows      int
	maxInputRows int
	inlineDiff   bool
	columnsFn    func() int
	rows         []string
	styles       []TextStyle
	spans        [][]textSpan
	cursorRow    int
	cursorCol    int
	renderedCols int
	plainSeen    map[string]struct{}
	stream       liveStream
	committed    bool
	lastCommit   commitKind
	streamBlock  bool
	closed       bool
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
		maxRows = DefaultLiveRows
	}
	maxInputRows := config.MaxInputRows
	if maxInputRows <= 0 {
		maxInputRows = defaultInputRows
	}
	return &LiveRenderer{
		w:            w,
		tty:          config.TTY,
		color:        config.Color && config.TTY,
		columns:      columns,
		maxRows:      maxRows,
		maxInputRows: maxInputRows,
		inlineDiff:   config.InlineDiff,
		columnsFn:    config.ColumnsFunc,
		plainSeen:    make(map[string]struct{}),
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

// Columns returns the width used to lay out the next live frame.
func (r *LiveRenderer) Columns() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncColumns()
	return r.columns
}

// MaxRows returns the live region's row budget for transient status rows. It
// bounds how tall activity can grow before the renderer clips its oldest rows.
func (r *LiveRenderer) MaxRows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxRows
}

// Marquee returns one display-width-bounded window moving from right to left.
func Marquee(value string, width, offset int) string {
	if width <= 0 {
		return ""
	}
	value = strings.Join(strings.Fields(Sanitize(value)), " ")
	if displayWidth(value) <= width {
		return value
	}
	gap := strings.Repeat(" ", min(3, width))
	runes := []rune(value + gap)
	if len(runes) == 0 {
		return ""
	}
	offset %= len(runes)
	if offset < 0 {
		offset += len(runes)
	}
	var out strings.Builder
	used := 0
	for i := 0; i < len(runes) && used < width; i++ {
		value := runes[(offset+i)%len(runes)]
		next := runeWidth(value)
		if used+next > width {
			break
		}
		out.WriteRune(value)
		used += next
	}
	return out.String()
}

// Update replaces the live region. Input is sanitized and physically wrapped.
func (r *LiveRenderer) Update(lines []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	rows := r.layoutLines(lines)
	if !r.tty {
		return r.writePlain(rows)
	}
	row, col := lastPosition(rows)
	return r.redraw(rows, row, col)
}

// UpdateStyled replaces the live region with styled, sanitized, wrapped text.
func (r *LiveRenderer) UpdateStyled(lines []StyledText) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	content, styles := r.layoutStyledLinesRich(lines)
	if !r.tty {
		return r.writePlain(content.rows)
	}
	row, col := lastPosition(content.rows)
	return r.redrawRich(content.rows, styles, content.spans, row, col)
}

// Prompt redraws the independent input region and any completion choices.
func (r *LiveRenderer) Prompt(state PromptState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()

	content, cursorRow, cursorCol := r.promptRows(state, r.maxInputRows)
	if !r.tty {
		return r.writePlain(content.rows)
	}
	return r.redrawRich(content.rows, nil, content.spans, cursorRow, cursorCol)
}

// Frame redraws one composite response, pending queue, and editor while keeping
// the cursor in the editor. It remains the compatibility entry point for an
// isolated frame; task-aware callers should use Frames.
func (r *LiveRenderer) Frame(frame LiveFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frame(frame)
}

// Frames redraws task-scoped frames in post-order: a task's descendants appear
// before its MainStatus frame. Sibling order follows first appearance in the
// input, and frames whose parent is absent are rendered as independent roots.
func (r *LiveRenderer) Frames(frames []LiveFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ordered, err := orderLiveFrames(frames)
	if err != nil {
		return err
	}
	return r.frame(mergeLiveFrames(ordered))
}

func orderLiveFrames(frames []LiveFrame) ([]LiveFrame, error) {
	type taskFrames struct {
		session  string
		parent   string
		ordinary []LiveFrame
		status   *LiveFrame
	}
	tasks := make(map[string]*taskFrames)
	order := make([]string, 0)
	for i := range frames {
		frame := frames[i]
		if frame.TaskID == "" {
			return nil, errFrameTaskIDEmpty
		}
		key := frame.TaskID
		task := tasks[key]
		if task == nil {
			task = &taskFrames{session: frame.SessionID, parent: frame.ParentSessionID}
			tasks[key] = task
			order = append(order, key)
		} else {
			if task.session == "" {
				task.session = frame.SessionID
			}
			if task.parent == "" {
				task.parent = frame.ParentSessionID
			}
		}
		if frame.MainStatus {
			if task.status != nil {
				return nil, errFrameStatusConflict
			}
			task.status = &frame
		} else {
			task.ordinary = append(task.ordinary, frame)
		}
	}
	// A shell task runs within its owning agent's session and therefore has the
	// same SessionID and ParentSessionID. Prefer the non-self-parented task as
	// that session's ancestry node, regardless of frame arrival order.
	sessionOwners := make(map[string]string)
	for _, id := range order {
		task := tasks[id]
		if task.session == "" {
			continue
		}
		owner := sessionOwners[task.session]
		if owner == "" || tasks[owner].parent == task.session && task.parent != task.session {
			sessionOwners[task.session] = id
		}
	}
	children := make(map[string][]string)
	for _, id := range order {
		parent := sessionOwners[tasks[id].parent]
		if parent != "" && parent != id {
			children[parent] = append(children[parent], id)
		}
	}
	state := make(map[string]uint8)
	ordered := make([]LiveFrame, 0, len(frames))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return errFrameTaskCycle
		case 2:
			return nil
		}
		state[id] = 1
		task := tasks[id]
		for _, child := range children[id] {
			if err := visit(child); err != nil {
				return err
			}
		}
		ordered = append(ordered, task.ordinary...)
		if task.status != nil {
			ordered = append(ordered, *task.status)
		}
		state[id] = 2
		return nil
	}
	for _, id := range order {
		parent := sessionOwners[tasks[id].parent]
		if parent == "" || parent == id {
			if err := visit(id); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range order {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// mergeLiveFrames combines already ordered task portions into the composite
// representation used by the terminal layout engine. The final status frame
// owns the shared stream, modeline, pending queue, and editor chrome.
func mergeLiveFrames(frames []LiveFrame) LiveFrame {
	var merged LiveFrame
	chrome := -1
	for i, frame := range frames {
		merged.Context = append(merged.Context, frame.Context...)
		merged.Activity = append(merged.Activity, frame.Activity...)
		merged.StyledActivity = append(merged.StyledActivity, frame.StyledActivity...)
		if frame.Message != "" || frame.MessagePrefix != "" {
			merged.MessagePrefix = frame.MessagePrefix
			merged.Message = frame.Message
		}
		if frame.Stream != nil {
			merged.Stream = frame.Stream
		}
		if frame.MainStatus && (chrome < 0 || liveFrameOwnsChrome(frame)) {
			chrome = i
		}
	}
	if chrome < 0 {
		return merged
	}
	frame := frames[chrome]
	merged.TaskID = frame.TaskID
	merged.SessionID = frame.SessionID
	merged.ParentSessionID = frame.ParentSessionID
	merged.MainStatus = true
	merged.PromptContext = frame.PromptContext
	merged.InputLeft = frame.InputLeft
	merged.InputCenter = frame.InputCenter
	merged.InputRight = frame.InputRight
	merged.Pending = frame.Pending
	merged.Prompt = frame.Prompt
	merged.Busy = frame.Busy
	merged.Spinner = frame.Spinner
	merged.ShowDivider = frame.ShowDivider
	return merged
}

func liveFrameOwnsChrome(frame LiveFrame) bool {
	return frame.Prompt.Prefix != "" || frame.Prompt.Text != "" || len(frame.Prompt.Completions) != 0 ||
		len(frame.PromptContext) != 0 || len(frame.Pending) != 0 || frame.InputLeft != "" ||
		frame.InputCenter != "" || frame.InputRight != "" || frame.Stream != nil || frame.Busy || frame.ShowDivider
}

func (r *LiveRenderer) frame(frame LiveFrame) error {
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	streamBefore := cloneLiveStream(r.stream)
	var promoted, streamContent richRows
	if frame.Stream != nil {
		var err error
		promoted, streamContent, err = r.advanceStream(*frame.Stream, false)
		if err != nil {
			r.stream = streamBefore
			return err
		}
	}
	streamContent = streamContent.tail(r.maxRows)
	streamRows := streamContent.rows
	streamSpans := streamContent.spans
	prompt := frame.Prompt
	if frame.Busy {
		spinner := frame.Spinner
		if spinner == "" {
			spinner = "*"
		}
		prompt.Prefix = spinner + " " + prompt.Prefix
	}
	// The modeline is chrome between the live and input regions. It does not
	// consume the input region's twelve-row prompt/menu budget.
	promptContent, promptCursorRow, promptCursorCol := r.promptRows(prompt, r.maxInputRows)
	promptRows := promptContent.rows
	var promptContextRows []string
	for _, item := range frame.PromptContext {
		promptContextRows = append(promptContextRows, r.layoutLines([]string{item})...)
	}

	dividerRows := 0
	if frame.ShowDivider {
		dividerRows = 1
	}
	barRows := 0
	if !frame.ShowDivider && (frame.InputLeft != "" || frame.InputRight != "") {
		barRows = 1
	}
	// Pending previews use only spare input capacity and never reduce the
	// prompt/menu's own row budget.
	available := max(0, r.maxInputRows-len(promptRows))
	pendingCount := min(len(frame.Pending), min(2, available))
	var pendingRows []string
	for i := len(frame.Pending) - 1; i >= len(frame.Pending)-pendingCount; i-- {
		item := frame.Pending[i]
		line, _, _ := strings.Cut(strings.TrimSpace(Sanitize(item)), "\n")
		group := []string{pendingPreview(line, r.columns)}
		if len(pendingRows) >= available {
			continue
		}
		pendingRows = append(group, pendingRows...)
	}

	inputRows := make([]string, 0, dividerRows+barRows+len(pendingRows)+len(promptContextRows)+len(promptRows))
	if frame.ShowDivider {
		inputRows = append(inputRows, dividerStatusBar(frame.InputLeft, frame.InputCenter, frame.InputRight, r.columns))
	}
	if barRows > 0 {
		// The labels are the modeline, not a separate status row. Keep its thin
		// rule even when the transcript boundary was already committed (notably
		// immediately after an assistant response).
		inputRows = append(inputRows, dividerStatusBar(frame.InputLeft, frame.InputCenter, frame.InputRight, r.columns))
	}
	inputRows = append(inputRows, pendingRows...)
	inputRows = append(inputRows, promptContextRows...)
	inputRows = append(inputRows, promptRows...)
	inputSpans := make([][]textSpan, dividerRows+barRows+len(pendingRows)+len(promptContextRows))
	inputSpans = append(inputSpans, promptContent.spans...)
	assistantMessage := frame.Message != "" && isAssistantPrefix(frame.MessagePrefix)
	assistantGap := !r.streamBlock && len(promoted.rows) == 0 && len(streamRows) > 0
	remaining := r.maxRows
	if assistantGap || assistantMessage {
		remaining = max(1, remaining-1)
	}

	// Keep the unfinished streaming row at the top of the live region. A row
	// promoted on the next frame is written at this same boundary before the
	// region is redrawn, so it moves into scrollback without jumping past
	// context or activity rows.
	if len(streamRows) > remaining {
		start := len(streamRows) - remaining
		streamRows = streamRows[start:]
		streamSpans = streamSpans[start:]
	}
	remaining -= len(streamRows)

	contextRows := make([]string, 0, len(frame.Context))
	for _, item := range frame.Context {
		itemRows, _ := r.layoutStyledContentAtColumns([]StyledText{{Text: item}}, r.columns)
		contextRows = append(contextRows, itemRows...)
	}
	if len(contextRows) > remaining {
		contextRows = contextRows[:remaining]
	}
	remaining -= len(contextRows)

	var activity []string
	var activityStyles []TextStyle
	var activitySpans [][]textSpan
	messageRowCount := 0
	for _, item := range frame.Activity {
		itemRows, _ := r.layoutStyledContentAtColumns([]StyledText{{Text: item}}, r.columns)
		activity = append(activity, itemRows...)
		activityStyles = append(activityStyles, make([]TextStyle, len(itemRows))...)
		activitySpans = append(activitySpans, make([][]textSpan, len(itemRows))...)
	}
	for _, item := range frame.StyledActivity {
		content, itemStyles := r.layoutLiveStyledTextAtColumns(item, r.columns)
		activity = append(activity, content.rows...)
		activityStyles = append(activityStyles, itemStyles...)
		activitySpans = append(activitySpans, content.spans...)
	}
	if frame.Message != "" || frame.MessagePrefix != "" {
		messageRows := r.messageRowsAtColumns(frame.MessagePrefix, frame.Message, r.columns)
		messageRowCount = len(messageRows)
		activity = append(activity, messageRows...)
		messageStyle := TextStyleDefault
		if isAssistantPrefix(frame.MessagePrefix) {
			messageStyle = textStyleAssistantMessage
		}
		activityStyles = append(activityStyles, repeatedStyle(messageStyle, len(messageRows))...)
		activitySpans = append(activitySpans, make([][]textSpan, len(messageRows))...)
	}
	if len(activity) > remaining {
		start := len(activity) - remaining
		activity = activity[start:]
		activityStyles = activityStyles[start:]
		activitySpans = activitySpans[start:]
	}
	if assistantMessage && len(activity) > 0 {
		messageStart := max(0, len(activity)-messageRowCount)
		activity = slices.Insert(activity, messageStart, "")
		activityStyles = slices.Insert(activityStyles, messageStart, TextStyleDefault)
		activitySpans = slices.Insert(activitySpans, messageStart, nil)
	}
	rows := append(streamRows, contextRows...)
	rows = append(rows, activity...)
	styles := repeatedStyle(textStyleAssistantMessage, len(streamRows))
	styles = append(styles, make([]TextStyle, len(contextRows))...)
	styles = append(styles, activityStyles...)
	spans := append([][]textSpan(nil), streamSpans...)
	spans = append(spans, make([][]textSpan, len(contextRows))...)
	spans = append(spans, activitySpans...)
	// Permanent transcript blocks and transient output are separate visual
	// regions. Keep an ordinary block gap before transient activity so it appears
	// immediately after a submitted user message or settled response.
	blockGap := 0
	if !r.streamBlock && len(promoted.rows) == 0 && (assistantGap || r.committed && !assistantMessage && r.lastCommit == commitBlock) && (len(rows) > 0 || len(inputRows) > 0) {
		rows = append([]string{""}, rows...)
		styles = append([]TextStyle{TextStyleDefault}, styles...)
		spans = append([][]textSpan{nil}, spans...)
		blockGap = 1
	}
	rows = append(rows, inputRows...)
	inputStyles := make([]TextStyle, len(inputRows))
	for i := 0; i < dividerRows+barRows; i++ {
		inputStyles[i] = textStyleGreen
	}
	styles = append(styles, inputStyles...)
	spans = append(spans, inputSpans...)
	cursorRow := len(streamRows) + len(contextRows) + len(activity) + blockGap + dividerRows + barRows + len(pendingRows) + len(promptContextRows) + promptCursorRow
	if !r.tty {
		if len(promoted.rows) > 0 && !r.streamBlock {
			promoted.rows = append([]string{""}, promoted.rows...)
		}
		if err := r.writePlain(append(promoted.rows, rows...)); err != nil {
			r.stream = streamBefore
			return err
		}
		return nil
	}
	if len(promoted.rows) > 0 {
		if err := r.promoteAndRedraw(promoted, rows, styles, spans, cursorRow, promptCursorCol); err != nil {
			r.stream = streamBefore
			return err
		}
		return nil
	}
	if err := r.redrawRich(rows, styles, spans, cursorRow, promptCursorCol); err != nil {
		r.stream = streamBefore
		return err
	}
	return nil
}

// CommitStream commits only the suffix that has not already been promoted by
// Frame, followed by an optional divider.
func (r *LiveRenderer) CommitStream(message StreamMessage, divider bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commitStream(message, divider)
}

// CommitDisplayedStream commits the assistant stream exactly as it was last
// displayed. It is a recovery path for callers whose authoritative final text
// no longer extends disposable live deltas that may already be in scrollback.
func (r *LiveRenderer) CommitDisplayedStream(messageID string, divider bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	messageID = Sanitize(messageID)
	if messageID == "" {
		return errStreamMessageIDEmpty
	}
	if r.stream.id != messageID {
		return errStreamMessageConflict
	}
	return r.commitStream(StreamMessage{ID: messageID, Prefix: r.stream.prefix, Text: r.stream.text}, divider)
}

func (r *LiveRenderer) commitStream(message StreamMessage, divider bool) error {
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	before := cloneLiveStream(r.stream)
	content, _, err := r.advanceStream(message, true)
	if err != nil {
		r.stream = before
		return err
	}
	if divider {
		content.rows = append(content.rows, strings.Repeat("─", max(3, r.columns-1)))
		content.spans = append(content.spans, nil)
	}
	styles := repeatedStyle(textStyleAssistantMessage, len(content.rows))
	if divider {
		styles[len(styles)-1] = TextStyleDefault
	}
	if r.streamBlock {
		err = r.commitRowsRich(content.rows, styles, content.spans)
	} else {
		content.rows = append([]string{""}, content.rows...)
		styles = append([]TextStyle{TextStyleDefault}, styles...)
		content.spans = append([][]textSpan{nil}, content.spans...)
		err = r.commitRowsRich(content.rows, styles, content.spans)
	}
	if err != nil {
		r.stream = before
		return err
	}
	r.stream = liveStream{}
	r.streamBlock = false
	r.committed = true
	r.lastCommit = commitBlock
	return nil
}

func (r *LiveRenderer) promptRows(state PromptState, limit int) (richRows, int, int) {
	if state.MaxRows > 0 {
		limit = min(limit, state.MaxRows)
	}
	prefix := Sanitize(state.Prefix)
	cleanText := Sanitize(state.Text)
	text := prefix + cleanText
	cursor := runeCount(prefix) + clamp(state.Cursor, 0, runeCount(cleanText))
	indent := strings.Repeat(" ", displayWidth(prefix))
	if displayWidth(indent) >= r.columns {
		indent = ""
	}
	rows, cursorRow, cursorCol := layoutTextHanging(text, cursor, r.columns, indent)
	spans := promptTextSpans(rows, prefix, indent, r.columns)
	if len(rows) >= limit {
		start := cursorRow - limit + 1
		if start < 0 {
			start = 0
		}
		if start+limit > len(rows) {
			start = len(rows) - limit
		}
		rows = rows[start : start+limit]
		spans = spans[start : start+limit]
		cursorRow = clamp(cursorRow-start, 0, len(rows)-1)
		return richRows{rows: rows, spans: spans}, cursorRow, cursorCol
	}
	choiceBudget := limit - len(rows)
	total := len(state.Completions)
	if choiceBudget <= 0 || total == 0 {
		return richRows{rows: rows, spans: spans}, cursorRow, cursorCol
	}
	selected := clamp(state.Selected, 0, total-1)
	visible := min(total, choiceBudget)
	showMore := total > choiceBudget
	if showMore && choiceBudget == 1 {
		candidate := state.Completions[selected]
		line := "> " + Sanitize(candidate.Value)
		line += fmt.Sprintf("  (%d options hidden)", total-1)
		rows = append(rows, truncateMenuLine(line, r.columns))
		spans = append(spans, nil)
		return richRows{rows: rows, spans: spans}, cursorRow, cursorCol
	}
	if showMore {
		visible-- // reserve a row for an explicit viewport/overflow message
	}
	start := selected - visible + 1
	if start < 0 {
		start = 0
	}
	if start+visible > total {
		start = total - visible
	}
	end := start + visible
	for i := start; i < end; i++ {
		candidate := state.Completions[i]
		marker := "  "
		if i == selected {
			marker = "> "
		}
		line := marker + Sanitize(candidate.Value)
		if candidate.Description != "" {
			line += "  " + Sanitize(candidate.Description)
		}
		rows = append(rows, truncateMenuLine(line, r.columns))
		spans = append(spans, nil)
	}
	if showMore && choiceBudget > 1 {
		hidden := total - visible
		line := fmt.Sprintf("Showing %d-%d of %d options; %d hidden", start+1, end, total, hidden)
		rows = append(rows, truncateMenuLine(line, r.columns))
		spans = append(spans, nil)
	}
	return richRows{rows: rows, spans: spans}, cursorRow, cursorCol
}

func promptTextSpans(rows []string, prefix, indent string, columns int) [][]textSpan {
	spans := make([][]textSpan, len(rows))
	if len(rows) == 0 {
		return spans
	}
	prefixRows, startRow, _ := layoutTextHanging(prefix, runeCount(prefix), columns, indent)
	green := ansiStyle{color: "32"}
	for row := startRow; row < len(rows); row++ {
		start := len(indent)
		if row == startRow {
			start = len(prefixRows[startRow])
		}
		if start < len(rows[row]) {
			spans[row] = []textSpan{{start: start, end: len(rows[row]), style: green}}
		}
	}
	return spans
}

func truncateMenuLine(line string, columns int) string {
	width := max(1, columns-1)
	if displayWidth(line) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	return truncateWidth(line, width-1) + "…"
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

func dividerStatusBar(left, center, right string, columns int) string {
	width := max(1, columns-1)
	if width < 3 {
		return strings.Repeat("─", width)
	}
	values := make([]string, 0, 2)
	if left = Sanitize(left); left != "" {
		values = append(values, left)
	}
	if center = Sanitize(center); center != "" {
		values = append(values, center)
	}
	right = Sanitize(right)
	if len(values) == 0 && right == "" {
		return strings.Repeat("─", width)
	}

	leftPart := ""
	if len(values) > 0 {
		leftPart = "─ " + strings.Join(values, " ─ ") + " "
	}
	if right == "" {
		if displayWidth(leftPart) >= width {
			return truncateWidth(leftPart, width-1) + "─"
		}
		return leftPart + strings.Repeat("─", width-displayWidth(leftPart))
	}

	// The model label belongs to the right edge of the modeline, independently
	// of the mode and transient center content grouped at the left edge.
	rightPart := " " + right + " "
	if displayWidth(rightPart) >= width {
		return "─" + truncateWidth(rightPart, width-1)
	}
	leftWidth := min(displayWidth(leftPart), width-displayWidth(rightPart))
	leftPart = truncateWidth(leftPart, leftWidth)
	return leftPart + strings.Repeat("─", width-leftWidth-displayWidth(rightPart)) + rightPart
}

func pendingPreview(value string, columns int) string {
	const prefix = UserPromptIcon + " "
	const suffix = "  (○ pending)"
	available := columns - displayWidth(prefix) - displayWidth(suffix)
	if available <= 0 {
		return truncateWidth(prefix+"pending", max(1, columns-1))
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
		return errRendererClosed
	}
	r.syncColumns()
	rows := r.messageRows(prefix, text)
	if len(rows) > r.maxRows {
		rows = rows[len(rows)-r.maxRows:]
	}
	if isAssistantPrefix(prefix) {
		rows = append([]string{""}, rows...)
	}
	if !r.tty {
		return r.writePlain(rows)
	}
	row, col := lastPosition(rows)
	if isAssistantPrefix(prefix) {
		return r.redrawStyled(rows, repeatedStyle(textStyleAssistantMessage, len(rows)), row, col)
	}
	return r.redraw(rows, row, col)
}

// CommitMessage appends a permanent hanging-indented message. Assistant
// messages use a role-specific foreground. When divider is true,
// a dim rule is committed beneath the message before the live response begins.
func (r *LiveRenderer) CommitMessage(prefix, text string, divider bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	if isAssistantPrefix(prefix) {
		content := renderAssistantMarkdown(prefix, text, r.columns, r.color)
		if divider {
			content.rows = append(content.rows, strings.Repeat("─", max(1, r.columns-1)))
			content.spans = append(content.spans, nil)
		}
		styles := repeatedStyle(textStyleAssistantMessage, len(content.rows))
		if divider {
			styles[len(styles)-1] = TextStyleDefault
		}
		content.rows = append([]string{""}, content.rows...)
		styles = append([]TextStyle{TextStyleDefault}, styles...)
		content.spans = append([][]textSpan{nil}, content.spans...)
		if err := r.commitRowsRich(content.rows, styles, content.spans); err != nil {
			return err
		}
		r.committed = true
		r.lastCommit = commitBlock
		return nil
	}
	rows := r.messageRows(prefix, text)
	if divider {
		rows = append(rows, strings.Repeat("─", max(1, r.columns-1)))
	}
	return r.commitRowsAs(rows, commitBlock)
}

// CommitUserMessage appends a permanent user message with a role-specific
// foreground.
func (r *LiveRenderer) CommitUserMessage(prefix, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	rows := append([]string{""}, r.messageRows(prefix, text)...)
	styles := append([]TextStyle{TextStyleDefault}, repeatedStyle(textStyleUserMessage, len(rows)-1)...)
	if err := r.commitRowsStyled(rows, styles); err != nil {
		return err
	}
	r.committed = true
	r.lastCommit = commitBlock
	return nil
}

// CommitBlock appends permanent multiline output as one spaced transcript block.
func (r *LiveRenderer) CommitBlock(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	clean := strings.TrimRight(Sanitize(text), "\r\n")
	rows := make([]string, 0)
	for _, line := range strings.Split(clean, "\n") {
		rows = append(rows, wrapLine(line, r.columns)...)
	}
	return r.commitRowsAs(rows, commitBlock)
}

// CommitStyled appends one compact styled report after sanitizing and wrapping.
func (r *LiveRenderer) CommitStyled(text StyledText) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	content, styles := r.layoutStyledTextAtColumns(text, r.columns)
	return r.commitRowsAsRich(content.rows, styles, content.spans, commitCompact)
}

// CommitStyledBlock appends one spaced styled multiline report.
func (r *LiveRenderer) CommitStyledBlock(text StyledText) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	content, styles := r.layoutStyledTextAtColumns(text, r.columns)
	return r.commitRowsAsRich(content.rows, styles, content.spans, commitBlock)
}

// CommitCodeBlock atomically appends a styled status and syntax-highlighted code
// as one transcript block. Unknown and plain-text languages fall back to
// unstyled code.
func (r *LiveRenderer) CommitCodeBlock(status StyledText, code, language string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	content, styles := r.layoutStyledTextAtColumns(status, r.columns)
	// A final newline terminates the last source line rather than introducing
	// another row. Remove only that terminator so selected trailing blank lines
	// remain visible.
	clean := strings.TrimSuffix(Sanitize(code), "\n")
	codeRows := renderCodeBlock("", markdownState{
		lexer:     lexerForLanguage(Sanitize(language)),
		code:      strings.Split(clean, "\n"),
		codeBytes: len(clean),
	}, r.columns, r.color)
	content.append(codeRows)
	styles = append(styles, repeatedStyle(TextStyleDefault, len(codeRows.rows))...)
	return r.commitRowsAsRich(content.rows, styles, content.spans, commitBlock)
}

// CommitDiffBlock atomically appends a styled status and a bounded rendering of
// raw unified diff text as one transcript block.
func (r *LiveRenderer) CommitDiffBlock(status StyledText, rawDiff string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRendererClosed
	}
	r.syncColumns()
	content, styles := r.layoutStyledTextAtColumns(status, r.columns)
	diff := formatDiff(rawDiff, r.columns, r.inlineDiff)
	content.rows = append(content.rows, diff.rows...)
	content.spans = append(content.spans, diff.spans...)
	styles = append(styles, repeatedStyle(TextStyleDefault, len(diff.rows))...)
	return r.commitRowsAsRich(content.rows, styles, content.spans, commitBlock)
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
		return errRendererClosed
	}
	r.syncColumns()
	clean := Sanitize(text)
	var output strings.Builder
	if r.tty && len(r.rows) > 0 {
		r.buildRedraw(&output, nil, 0, 0)
	}
	if r.committed && r.lastCommit == commitBlock {
		output.WriteByte('\n')
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
	r.styles = nil
	r.spans = nil
	r.cursorRow = 0
	r.cursorCol = 0
	r.committed = true
	r.lastCommit = commitCompact
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
			r.styles = nil
			r.spans = nil
		}
	}
	r.closed = true
	return err
}

func (r *LiveRenderer) redraw(rows []string, cursorRow, cursorCol int) error {
	return r.redrawStyled(rows, nil, cursorRow, cursorCol)
}

func (r *LiveRenderer) redrawStyled(rows []string, styles []TextStyle, cursorRow, cursorCol int) error {
	return r.redrawRich(rows, styles, nil, cursorRow, cursorCol)
}

func (r *LiveRenderer) redrawRich(rows []string, styles []TextStyle, spans [][]textSpan, cursorRow, cursorCol int) error {
	if r.sameFrame(rows, styles, spans, cursorRow, cursorCol) {
		return nil
	}
	var output strings.Builder
	r.buildRedrawRich(&output, rows, styles, spans, cursorRow, cursorCol)
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = append(r.rows[:0], rows...)
	r.styles = append(r.styles[:0], styles...)
	r.spans = cloneTextSpans(spans)
	r.cursorRow = cursorRow
	r.cursorCol = cursorCol
	r.renderedCols = r.columns
	return nil
}

// sameFrame compares the complete terminal-visible state. Comparing only text
// would incorrectly suppress cursor moves, style changes, or redraws after a
// terminal resize.
func (r *LiveRenderer) sameFrame(rows []string, styles []TextStyle, spans [][]textSpan, cursorRow, cursorCol int) bool {
	if r.renderedCols != r.columns || r.cursorRow != cursorRow || r.cursorCol != cursorCol || len(r.rows) != len(rows) {
		return false
	}
	for i := range rows {
		if r.rows[i] != rows[i] || r.visibleStyleAt(r.styles, i) != r.visibleStyleAt(styles, i) ||
			!r.visibleSpansEqual(spansAt(r.spans, i), spansAt(spans, i)) {
			return false
		}
	}
	return true
}

func (r *LiveRenderer) visibleSpansEqual(left, right []textSpan) bool {
	if !r.color {
		return true
	}
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (r *LiveRenderer) visibleStyleAt(styles []TextStyle, index int) TextStyle {
	if !r.color {
		return TextStyleDefault
	}
	return styleAt(styles, index)
}

func (r *LiveRenderer) buildRedraw(output *strings.Builder, rows []string, cursorRow, cursorCol int) {
	r.buildRedrawStyled(output, rows, nil, cursorRow, cursorCol)
}

func (r *LiveRenderer) buildRedrawStyled(output *strings.Builder, rows []string, styles []TextStyle, cursorRow, cursorCol int) {
	r.buildRedrawRich(output, rows, styles, nil, cursorRow, cursorCol)
}

func (r *LiveRenderer) buildRedrawRich(output *strings.Builder, rows []string, styles []TextStyle, spans [][]textSpan, cursorRow, cursorCol int) {
	// Cursor visibility changes surround one buffered write, not persistent UI.
	output.WriteString("\x1b[?25l")
	if len(r.rows) > 0 {
		output.WriteByte('\r')
		moveUp(output, physicalCursorRow(r.rows, r.cursorRow, r.cursorCol, r.columns))
	}
	count := max(physicalRowCount(r.rows, r.columns), len(rows))
	for i := 0; i < count; i++ {
		if i < len(rows) && r.color {
			// EL paints erased cells with the active background, so the live
			// surface extends across the complete terminal row without padding
			// it to the auto-wrap boundary.
			output.WriteString("\x1b[" + liveBackgroundANSI + "m\x1b[2K\x1b[0m")
			output.WriteString(r.decorateRichOnSurface(rows[i], styleAt(styles, i), spansAt(spans, i), surfaceLive))
		} else {
			output.WriteString("\x1b[2K")
			if i < len(rows) {
				output.WriteString(r.decorateRich(rows[i], styleAt(styles, i), spansAt(spans, i)))
			}
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
			prefix := prefixWidth(rows[cursorRow], cursorCol)
			output.WriteString(r.decorateRichOnSurface(prefix, styleAt(styles, cursorRow), clipTextSpans(spansAt(spans, cursorRow), len(prefix)), surfaceLive))
		}
	}
	output.WriteString("\x1b[?25h")
}

func (r *LiveRenderer) commitRows(rows []string) error {
	return r.commitRowsStyled(rows, nil)
}

func (r *LiveRenderer) commitRowsStyled(rows []string, styles []TextStyle) error {
	return r.commitRowsRich(rows, styles, nil)
}

func (r *LiveRenderer) commitRowsRich(rows []string, styles []TextStyle, spans [][]textSpan) error {
	var output strings.Builder
	if r.tty && len(r.rows) > 0 {
		r.buildRedraw(&output, nil, 0, 0)
	}
	for i, row := range rows {
		output.WriteString(r.decorateRich(row, styleAt(styles, i), spansAt(spans, i)))
		output.WriteByte('\n')
	}
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = nil
	r.styles = nil
	r.spans = nil
	r.cursorRow = 0
	r.cursorCol = 0
	return nil
}

func (r *LiveRenderer) commitRowsAs(rows []string, kind commitKind) error {
	return r.commitRowsAsStyled(rows, nil, kind)
}

func (r *LiveRenderer) commitRowsAsStyled(rows []string, styles []TextStyle, kind commitKind) error {
	return r.commitRowsAsRich(rows, styles, nil, kind)
}

func (r *LiveRenderer) commitRowsAsRich(rows []string, styles []TextStyle, spans [][]textSpan, kind commitKind) error {
	if r.committed && (r.lastCommit == commitBlock || kind == commitBlock) {
		rows = append([]string{""}, rows...)
		styles = append([]TextStyle{TextStyleDefault}, styles...)
		spans = append([][]textSpan{nil}, spans...)
	}
	if err := r.commitRowsRich(rows, styles, spans); err != nil {
		return err
	}
	r.committed = true
	r.lastCommit = kind
	return nil
}

func cloneLiveStream(value liveStream) liveStream {
	value.pending = append([]rune(nil), value.pending...)
	value.markdown.code = append([]string(nil), value.markdown.code...)
	value.markdown.table.candidate = append([]string(nil), value.markdown.table.candidate...)
	value.markdown.table.header = append([]string(nil), value.markdown.table.header...)
	value.markdown.table.align = append([]tableAlignment(nil), value.markdown.table.align...)
	value.markdown.table.rows = cloneTableRows(value.markdown.table.rows)
	return value
}

func (r *LiveRenderer) advanceStream(message StreamMessage, complete bool) (richRows, richRows, error) {
	message.ID = Sanitize(message.ID)
	message.Prefix = Sanitize(message.Prefix)
	clean := Sanitize(message.Text)
	if message.ID == "" {
		return richRows{}, richRows{}, errStreamMessageIDEmpty
	}
	if r.stream.id == "" {
		r.stream = liveStream{id: message.ID, prefix: message.Prefix}
	} else if r.stream.id != message.ID {
		return richRows{}, richRows{}, errStreamMessageConflict
	} else if r.stream.prefix != message.Prefix {
		return richRows{}, richRows{}, errStreamPrefixChanged
	}
	if !strings.HasPrefix(clean, r.stream.text) {
		return richRows{}, richRows{}, errStreamTextChanged
	}
	r.stream.pending = append(r.stream.pending, []rune(clean[len(r.stream.text):])...)
	r.stream.text = clean

	var promoted richRows
	for {
		newline := -1
		for index, value := range r.stream.pending {
			if value == '\n' {
				newline = index
				break
			}
		}
		if newline < 0 {
			break
		}
		line := string(r.stream.pending[:newline])
		r.stream.pending = append([]rune(nil), r.stream.pending[newline+1:]...)
		rendered := r.renderStreamLine(line, &r.stream.markdown, r.stream.started)
		promoted.append(rendered)
		if len(rendered.rows) > 0 {
			r.stream.started = true
		}
	}

	if complete {
		if len(r.stream.pending) > 0 {
			rendered := r.renderStreamLine(string(r.stream.pending), &r.stream.markdown, r.stream.started)
			promoted.append(rendered)
			r.stream.pending = nil
			if len(rendered.rows) > 0 {
				r.stream.started = true
			}
		}
		// An unterminated fence is still response content. Flush its buffered
		// source as code when the provider completes instead of losing it while
		// waiting for a closing delimiter that can no longer arrive.
		if r.stream.markdown.inFence {
			if !r.stream.markdown.plainFence {
				rendered := renderCodeBlock(markdownPrefix(r.stream.prefix, r.stream.started), r.stream.markdown, r.columns, r.color)
				promoted.append(rendered)
				if len(rendered.rows) > 0 {
					r.stream.started = true
				}
			}
			r.stream.markdown = markdownState{}
		}
		if rendered := flushMarkdownTable(&r.stream.markdown, r.columns); len(rendered.rows) > 0 {
			promoted.append(rendered)
			r.stream.started = true
		}
		return promoted, richRows{}, nil
	}

	prefix := markdownPrefix(r.stream.prefix, r.stream.started)
	return promoted, r.renderStreamPreview(prefix), nil
}

// renderStreamPreview bounds Markdown parsing, syntax highlighting, and layout
// before Frame applies its row cap. Completed content is never truncated: this
// limit applies only to the redrawable view of an unfinished source line or an
// open fence.
func (r *LiveRenderer) renderStreamPreview(prefix string) richRows {
	state := r.stream.markdown
	if state.inFence {
		if len(r.stream.pending) > maxLiveMarkdownRunes {
			return renderPlainLiveRunesTail(prefix, r.stream.pending, r.columns, r.maxRows)
		}
		pending := string(r.stream.pending)
		closing := pending != "" && isFenceClosing(pending, state.fenceChar, state.fenceLength)
		if state.plainFence {
			if closing || pending == "" {
				return richRows{}
			}
			return renderPlainLiveTail(prefix, pending, r.columns, r.maxRows)
		}
		return renderOpenFencePreview(prefix, state, pending, !closing, r.columns, r.maxRows, r.color)
	}

	if len(r.stream.pending) > maxLiveMarkdownRunes {
		return renderPlainLiveRunesTail(prefix, r.stream.pending, r.columns, r.maxRows)
	}
	pending := string(r.stream.pending)
	previewState := state
	live := richRows{}
	if pending != "" {
		live = renderMarkdownLine(prefix, pending, r.columns, &previewState, r.color)
	}
	if previewState.inFence {
		live = renderOpenFencePreview(prefix, previewState, "", false, r.columns, r.maxRows, r.color)
	} else if previewState.table.pending() {
		live.append(flushMarkdownTable(&previewState, r.columns))
	}
	return live.tail(r.maxRows)
}

func renderPlainLiveRunesTail(prefix string, value []rune, columns, maxRows int) richRows {
	start := max(0, len(value)-maxLiveMarkdownRunes)
	linePrefix := prefix
	if start > 0 {
		linePrefix = hangingIndent(prefix)
	}
	return layoutRichRuns(withMarkdownPrefix(linePrefix, []textRun{{text: string(value[start:])}}), hangingIndent(prefix), columns).tail(maxRows)
}

func renderOpenFencePreview(prefix string, state markdownState, pending string, includePending bool, columns, maxRows int, color bool) richRows {
	pendingRunes := utf8.RuneCountInString(pending)
	if includePending && pendingRunes > maxLiveMarkdownRunes {
		return renderPlainLiveTail(prefix, pending, columns, maxRows)
	}
	extraLines, extraBytes := 0, 0
	if includePending {
		extraLines, extraBytes = 1, len(pending)
	}
	if state.codeBytes+extraBytes <= maxLiveMarkdownRunes && len(state.code)+extraLines <= maxLiveMarkdownLines {
		preview := state
		preview.code = append([]string(nil), state.code...)
		if includePending {
			appendCodeLine(&preview, pending)
		}
		return renderCodeBlock(prefix, preview, columns, color).tail(maxRows)
	}

	lines := state.code
	if includePending {
		lines = append(append([]string(nil), state.code...), pending)
	}
	return renderPlainCodeTail(prefix, lines, columns, maxRows)
}

func renderPlainLiveTail(prefix, value string, columns, maxRows int) richRows {
	suffix, truncated := suffixRunes(value, maxLiveMarkdownRunes)
	linePrefix := prefix
	if truncated {
		linePrefix = hangingIndent(prefix)
	}
	return layoutRichRuns(withMarkdownPrefix(linePrefix, []textRun{{text: suffix}}), hangingIndent(prefix), columns).tail(maxRows)
}

func renderPlainCodeTail(prefix string, lines []string, columns, maxRows int) richRows {
	if len(lines) == 0 {
		return layoutRichRuns(withMarkdownPrefix(prefix, nil), hangingIndent(prefix), columns).tail(maxRows)
	}
	start := max(0, len(lines)-maxLiveMarkdownLines)
	selected := make([]string, 0, len(lines)-start)
	remaining := maxLiveMarkdownRunes
	truncatedFirst := false
	for index := len(lines) - 1; index >= start && remaining > 0; index-- {
		suffix, truncated := suffixRunes(lines[index], remaining)
		selected = append(selected, suffix)
		remaining -= utf8.RuneCountInString(suffix)
		if truncated {
			truncatedFirst = true
			start = index
			break
		}
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	omitted := start > 0 || truncatedFirst || len(selected) < len(lines)
	var output richRows
	for index, line := range selected {
		linePrefix := hangingIndent(prefix)
		if index == 0 && !omitted {
			linePrefix = prefix
		}
		output.append(layoutRichRuns(withMarkdownPrefix(linePrefix, []textRun{{text: line}}), hangingIndent(prefix), columns))
	}
	return output.tail(maxRows)
}

func suffixRunes(value string, limit int) (string, bool) {
	if limit <= 0 {
		return "", value != ""
	}
	end := len(value)
	start := end
	for count := 0; start > 0 && count < limit; count++ {
		_, size := utf8.DecodeLastRuneInString(value[:start])
		start -= size
	}
	return value[start:end], start > 0
}

func (r *LiveRenderer) renderStreamLine(line string, state *markdownState, started bool) richRows {
	if state.tableRowLimit == 0 {
		state.tableRowLimit = maxMarkdownTableRows
	}
	prefix := markdownPrefix(r.stream.prefix, started)
	return renderMarkdownLine(prefix, line, r.columns, state, r.color)
}

func (r *LiveRenderer) promoteAndRedraw(promoted richRows, rows []string, styles []TextStyle, spans [][]textSpan, cursorRow, cursorCol int) error {
	var output strings.Builder
	r.buildRedraw(&output, nil, 0, 0)
	if !r.streamBlock {
		promoted.rows = append([]string{""}, promoted.rows...)
		promoted.spans = append([][]textSpan{nil}, promoted.spans...)
	}
	for index, row := range promoted.rows {
		output.WriteString(r.decorateRich(row, textStyleAssistantMessage, spansAt(promoted.spans, index)))
		output.WriteByte('\n')
	}
	oldRows, oldStyles, oldSpans := r.rows, r.styles, r.spans
	oldCursorRow, oldCursorCol, oldRenderedCols := r.cursorRow, r.cursorCol, r.renderedCols
	r.rows, r.styles, r.spans, r.cursorRow, r.cursorCol = nil, nil, nil, 0, 0
	r.buildRedrawRich(&output, rows, styles, spans, cursorRow, cursorCol)
	r.rows, r.styles, r.spans = oldRows, oldStyles, oldSpans
	r.cursorRow, r.cursorCol, r.renderedCols = oldCursorRow, oldCursorCol, oldRenderedCols
	if err := writeAtomic(r.w, output.String()); err != nil {
		return err
	}
	r.rows = append(r.rows[:0], rows...)
	r.styles = append(r.styles[:0], styles...)
	r.spans = cloneTextSpans(spans)
	r.cursorRow = cursorRow
	r.cursorCol = cursorCol
	r.renderedCols = r.columns
	r.streamBlock = true
	return nil
}

func (r *LiveRenderer) messageRows(prefix, text string) []string {
	return r.messageRowsAtColumns(prefix, text, r.columns)
}

func (r *LiveRenderer) messageRowsAtColumns(prefix, text string, columns int) []string {
	prefix = Sanitize(prefix)
	text = prefix + strings.TrimRight(Sanitize(text), "\r\n")
	rows, _, _ := layoutTextHanging(text, runeCount(text), columns, strings.Repeat(" ", displayWidth(prefix)))
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
	return r.decorateStyled(row, TextStyleDefault)
}

func (r *LiveRenderer) decorateStyled(row string, style TextStyle) string {
	return r.decorateRich(row, style, nil)
}

func (r *LiveRenderer) decorateDefault(row string, base ansiStyle) string {
	color := func(code, value string) string {
		return ansiStyled(value, mergeANSIStyle(base, ansiStyle{color: code}))
	}
	switch {
	case hasSpinnerPrefix(row):
		spinner, rest := firstRune(row)
		if strings.HasPrefix(rest, " "+UserPromptIcon+" ") {
			return color("36", spinner) + " " + color("36", UserPromptIcon) + rest[len(" "+UserPromptIcon):]
		}
		return color("36", spinner) + rest
	case strings.HasPrefix(row, UserPromptIcon+" "):
		return color("36", UserPromptIcon) + row[len(UserPromptIcon):]
	case strings.HasPrefix(row, AssistantMessageIcon+" ") || strings.HasPrefix(row, "- "):
		marker, rest := firstRune(row)
		return color("32", marker) + rest
	case strings.HasPrefix(row, "✓ "):
		return color("32", "✓") + row[len("✓"):]
	case strings.HasPrefix(row, "✗ "):
		return color("31", row)
	case strings.HasPrefix(row, "○ ") || strings.HasPrefix(row, "◌ ") || strings.HasPrefix(row, "■ "):
		return color("2", row)
	case strings.Trim(row, "─") == "":
		return color("2;90", row)
	case strings.HasPrefix(row, "error:"):
		return color("31", row)
	case strings.HasPrefix(row, "status:") || strings.HasPrefix(row, "tool:"):
		return color("2", row)
	case strings.HasPrefix(row, "> "):
		return color("36", ">") + row[1:]
	default:
		return ansiStyled(row, base)
	}
}

func (r *LiveRenderer) decorateRich(row string, style TextStyle, spans []textSpan) string {
	return r.decorateRichOnSurface(row, style, spans, surfaceDefault)
}

func (r *LiveRenderer) decorateRichOnSurface(row string, style TextStyle, spans []textSpan, surface surfaceStyle) string {
	if !r.color || row == "" {
		return row
	}
	base := ansiStyle{}
	if surface == surfaceLive {
		base = ansiStyle{color: liveForegroundANSI, background: liveBackgroundANSI}
	}
	switch style {
	case TextStyleMuted:
		color := "90"
		if surface == surfaceLive {
			color = liveMutedForegroundANSI
		}
		return ansiStyled(row, mergeANSIStyle(base, ansiStyle{color: color}))
	case textStyleGreen:
		return ansiStyled(row, mergeANSIStyle(base, ansiStyle{color: "32"}))
	case textStyleUserMessage:
		return ansiStyled(row, mergeANSIStyle(base, ansiStyle{color: "32"}))
	case textStyleAssistantMessage:
		base.color = "38;5;195"
	}
	var semantic []textSpan
	if style == TextStyleDefault {
		semantic = semanticTextSpans(row)
	}
	if len(spans) == 0 && len(semantic) == 0 {
		return ansiStyled(row, base)
	}
	boundaries := []int{0, len(row)}
	for _, group := range [][]textSpan{spans, semantic} {
		for _, span := range group {
			boundaries = append(boundaries, clamp(span.start, 0, len(row)), clamp(span.end, 0, len(row)))
		}
	}
	slices.Sort(boundaries)
	var output strings.Builder
	for i := 1; i < len(boundaries); i++ {
		start, end := boundaries[i-1], boundaries[i]
		if start == end {
			continue
		}
		combined := mergeANSIStyle(base, spanStyleAt(semantic, start))
		combined = mergeANSIStyle(combined, spanStyleAt(spans, start))
		output.WriteString(ansiStyled(row[start:end], combined))
	}
	return output.String()
}

func semanticTextSpans(row string) []textSpan {
	span := func(end int, style ansiStyle) []textSpan { return []textSpan{{start: 0, end: end, style: style}} }
	cyan := ansiStyle{color: "36"}
	switch {
	case hasSpinnerPrefix(row):
		_, rest := firstRune(row)
		spinnerEnd := len(row) - len(rest)
		spans := span(spinnerEnd, cyan)
		if strings.HasPrefix(rest, " "+UserPromptIcon+" ") {
			start := spinnerEnd + len(" ")
			spans = append(spans, textSpan{start: start, end: start + len(UserPromptIcon), style: cyan})
		}
		return spans
	case strings.HasPrefix(row, UserPromptIcon+" "):
		return span(len(UserPromptIcon), cyan)
	case strings.HasPrefix(row, AssistantMessageIcon+" ") || strings.HasPrefix(row, "- "):
		_, rest := firstRune(row)
		return span(len(row)-len(rest), ansiStyle{color: "32"})
	case strings.HasPrefix(row, "✓ "):
		return span(len("✓"), ansiStyle{color: "32"})
	case strings.HasPrefix(row, "✗ "):
		return span(len(row), ansiStyle{color: "31"})
	case strings.HasPrefix(row, "○ ") || strings.HasPrefix(row, "◌ ") || strings.HasPrefix(row, "■ "):
		return span(len(row), ansiStyle{dim: true})
	case strings.Trim(row, "─") == "":
		return span(len(row), ansiStyle{color: "90", dim: true})
	case strings.HasPrefix(row, "error:"):
		return span(len(row), ansiStyle{color: "31"})
	case strings.HasPrefix(row, "status:") || strings.HasPrefix(row, "tool:"):
		return span(len(row), ansiStyle{dim: true})
	case strings.HasPrefix(row, "> "):
		return span(len(">"), cyan)
	default:
		return nil
	}
}

func spanStyleAt(spans []textSpan, position int) ansiStyle {
	for _, span := range spans {
		if position >= span.start && position < span.end {
			return span.style
		}
	}
	return ansiStyle{}
}

func ansiStyled(value string, style ansiStyle) string {
	if value == "" || style == (ansiStyle{}) {
		return value
	}
	codes := make([]string, 0, 6)
	if style.bold {
		codes = append(codes, "1")
	}
	if style.dim {
		codes = append(codes, "2")
	}
	if style.italic {
		codes = append(codes, "3")
	}
	if style.underline {
		codes = append(codes, "4")
	}
	if style.strike {
		codes = append(codes, "9")
	}
	if style.color != "" {
		codes = append(codes, style.color)
	}
	if style.background != "" {
		codes = append(codes, style.background)
	}
	if len(codes) == 0 {
		return value
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + value + "\x1b[0m"
}

func spansAt(spans [][]textSpan, index int) []textSpan {
	if index < 0 || index >= len(spans) {
		return nil
	}
	return spans[index]
}

func cloneTextSpans(spans [][]textSpan) [][]textSpan {
	if len(spans) == 0 {
		return nil
	}
	clone := make([][]textSpan, len(spans))
	for index := range spans {
		clone[index] = append([]textSpan(nil), spans[index]...)
	}
	return clone
}

func clipTextSpans(spans []textSpan, bytes int) []textSpan {
	var clipped []textSpan
	for _, span := range spans {
		if span.start >= bytes {
			break
		}
		if span.end > bytes {
			span.end = bytes
		}
		clipped = append(clipped, span)
	}
	return clipped
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
	styled := make([]StyledText, len(lines))
	for i, line := range lines {
		styled[i].Text = line
	}
	rows, _ := r.layoutStyledLines(styled)
	return rows
}

func (r *LiveRenderer) layoutStyledLines(lines []StyledText) ([]string, []TextStyle) {
	content, styles := r.layoutStyledLinesRich(lines)
	return content.rows, styles
}

func (r *LiveRenderer) layoutStyledLinesRich(lines []StyledText) (richRows, []TextStyle) {
	var content richRows
	var styles []TextStyle
	for _, line := range lines {
		item, itemStyles := r.layoutLiveStyledTextAtColumns(line, r.columns)
		content.append(item)
		styles = append(styles, itemStyles...)
	}
	if len(content.rows) > r.maxRows {
		start := len(content.rows) - r.maxRows
		content.rows = content.rows[start:]
		content.spans = content.spans[start:]
		styles = styles[start:]
	}
	return content, styles
}

func (r *LiveRenderer) layoutStyledContent(lines []StyledText) ([]string, []TextStyle) {
	return r.layoutStyledContentAtColumns(lines, r.columns)
}

func (r *LiveRenderer) layoutStyledContentAtColumns(lines []StyledText, columns int) ([]string, []TextStyle) {
	rows := make([]string, 0, len(lines))
	styles := make([]TextStyle, 0, len(lines))
	for _, line := range lines {
		for _, part := range strings.Split(Sanitize(line.Text), "\n") {
			indent := part[:len(part)-len(strings.TrimLeft(part, " "))]
			if displayWidth(indent) >= columns-1 {
				indent = ""
			}
			wrapped, _, _ := layoutTextHanging(part, runeCount(part), columns, indent)
			rows = append(rows, wrapped...)
			for range wrapped {
				styles = append(styles, line.Style)
			}
		}
	}
	return rows, styles
}

func (r *LiveRenderer) layoutStyledTextAtColumns(line StyledText, columns int) (richRows, []TextStyle) {
	if !line.Markdown {
		rows, styles := r.layoutStyledContentAtColumns([]StyledText{line}, columns)
		return richRows{rows: rows, spans: make([][]textSpan, len(rows))}, styles
	}
	content := renderMarkdown(line.Prefix, line.Text, line.Suffix, columns, r.color)
	styles := make([]TextStyle, len(content.rows))
	for i := range styles {
		styles[i] = line.Style
	}
	return content, styles
}

func (r *LiveRenderer) layoutLiveStyledTextAtColumns(line StyledText, columns int) (richRows, []TextStyle) {
	if !line.Markdown {
		return r.layoutStyledTextAtColumns(line, columns)
	}
	if preview, truncated := liveMarkdownPreview(line.Text); truncated {
		prefix := hangingIndent(Sanitize(line.Prefix))
		content := renderPlainCodeTail(prefix, strings.Split(Sanitize(preview+line.Suffix), "\n"), columns, r.maxRows)
		styles := make([]TextStyle, len(content.rows))
		for i := range styles {
			styles[i] = line.Style
		}
		return content, styles
	}
	return r.layoutStyledTextAtColumns(line, columns)
}

func liveMarkdownPreview(source string) (string, bool) {
	end := len(source)
	start := end
	lines := 1
	for runes := 0; start > 0 && runes < maxLiveMarkdownRunes; runes++ {
		value, size := utf8.DecodeLastRuneInString(source[:start])
		if value == '\n' {
			if lines == maxLiveMarkdownLines {
				break
			}
			lines++
		}
		start -= size
	}
	return source[start:end], start > 0
}

func styleAt(styles []TextStyle, index int) TextStyle {
	if index < 0 || index >= len(styles) {
		return TextStyleDefault
	}
	return styles[index]
}

func repeatedStyle(style TextStyle, count int) []TextStyle {
	styles := make([]TextStyle, count)
	for index := range styles {
		styles[index] = style
	}
	return styles
}

func isAssistantPrefix(prefix string) bool {
	prefix = Sanitize(prefix)
	return prefix == AssistantMessageIcon+" " || prefix == "- "
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
