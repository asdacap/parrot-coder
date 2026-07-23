package terminal

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrCanceled    = errors.New("terminal: canceled")
	ErrInterrupted = errors.New("terminal: interrupted")
	ErrInputLimit  = errors.New("terminal: input limit exceeded")
)

// Candidate is one command completion or picker option.
type Candidate struct {
	Value       string
	Description string
}

type editorConfig struct {
	prompt              string
	maxInputBytes       int
	maxInputRunes       int
	maxHistory          int
	maxCompletionRows   int
	history             []string
	completions         []Candidate
	renderer            *LiveRenderer
	disableAutoRenderer bool
}

// EditorOption customizes an Editor.
type EditorOption func(*editorConfig)

// WithEditorPrompt sets the prompt prefix.
func WithEditorPrompt(prompt string) EditorOption {
	return func(config *editorConfig) { config.prompt = prompt }
}

// WithEditorLimits sets input byte/rune, history, and completion row bounds.
// Non-positive values retain their defaults.
func WithEditorLimits(bytes, runes, history, completionRows int) EditorOption {
	return func(config *editorConfig) {
		if bytes > 0 {
			config.maxInputBytes = bytes
		}
		if runes > 0 {
			config.maxInputRunes = runes
		}
		if history > 0 {
			config.maxHistory = history
		}
		if completionRows > 0 {
			config.maxCompletionRows = completionRows
		}
	}
}

// WithEditorHistory supplies initial history, oldest first.
func WithEditorHistory(history []string) EditorOption {
	return func(config *editorConfig) { config.history = append([]string(nil), history...) }
}

// WithCompletions supplies slash-command completion candidates.
func WithCompletions(candidates []Candidate) EditorOption {
	return func(config *editorConfig) { config.completions = append([]Candidate(nil), candidates...) }
}

// WithEditorRenderer uses renderer for prompt redraws. Passing nil disables
// automatic rendering.
func WithEditorRenderer(renderer *LiveRenderer) EditorOption {
	return func(config *editorConfig) {
		config.renderer = renderer
		config.disableAutoRenderer = renderer == nil
	}
}

// Editor provides rune-aware multiline editing and slash completion.
type Editor struct {
	decoder           *KeyDecoder
	renderer          *LiveRenderer
	prompt            string
	maxInputBytes     int
	maxInputRunes     int
	maxHistory        int
	maxCompletionRows int
	history           []string
	completions       []Candidate
}

// NewEditor creates an editor over a combined input/output stream.
func NewEditor(rw io.ReadWriter, options ...EditorOption) *Editor {
	return NewEditorIO(rw, rw, options...)
}

// NewEditorIO creates an editor with separate input and output streams.
func NewEditorIO(reader io.Reader, writer io.Writer, options ...EditorOption) *Editor {
	return newEditor(NewKeyDecoder(reader), writer, options...)
}

// NewEditorDecoder creates an editor that shares a key decoder with other
// terminal controls. This preserves bytes already buffered by the decoder.
func NewEditorDecoder(decoder *KeyDecoder, writer io.Writer, options ...EditorOption) *Editor {
	return newEditor(decoder, writer, options...)
}

func newEditor(decoder *KeyDecoder, writer io.Writer, options ...EditorOption) *Editor {
	config := editorConfig{
		prompt:            "> ",
		maxInputBytes:     64 << 10,
		maxInputRunes:     16 << 10,
		maxHistory:        100,
		maxCompletionRows: DefaultLiveRows - 1,
	}
	for _, option := range options {
		option(&config)
	}
	if config.renderer == nil && !config.disableAutoRenderer && writer != nil {
		config.renderer = NewLiveRenderer(writer, RendererConfig{TTY: IsTTY(writer)})
	}
	editor := &Editor{
		decoder:           decoder,
		renderer:          config.renderer,
		prompt:            config.prompt,
		maxInputBytes:     config.maxInputBytes,
		maxInputRunes:     config.maxInputRunes,
		maxHistory:        config.maxHistory,
		maxCompletionRows: config.maxCompletionRows,
	}
	editor.decoder.SetMaxPasteBytes(config.maxInputBytes)
	editor.SetCompletions(config.completions)
	for _, entry := range config.history {
		editor.AddHistory(entry)
	}
	return editor
}

// SetCompletions replaces slash completion candidates. Invalid values are
// omitted and the remaining candidates are sorted for deterministic results.
func (e *Editor) SetCompletions(candidates []Candidate) {
	e.completions = e.completions[:0]
	for _, candidate := range candidates {
		if validSingleLine(candidate.Value) && validSingleLine(candidate.Description) && strings.HasPrefix(candidate.Value, "/") {
			e.completions = append(e.completions, candidate)
		}
	}
	sort.SliceStable(e.completions, func(i, j int) bool {
		if e.completions[i].Value == e.completions[j].Value {
			return e.completions[i].Description < e.completions[j].Description
		}
		return e.completions[i].Value < e.completions[j].Value
	})
}

// AddHistory appends a valid entry unless it duplicates the newest entry.
func (e *Editor) AddHistory(value string) {
	if value == "" || !validInput(value) || len(value) > e.maxInputBytes || runeCount(value) > e.maxInputRunes {
		return
	}
	if len(e.history) > 0 && e.history[len(e.history)-1] == value {
		return
	}
	e.history = append(e.history, value)
	if len(e.history) > e.maxHistory {
		e.history = append([]string(nil), e.history[len(e.history)-e.maxHistory:]...)
	}
}

// History returns a copy of the editor history, oldest first.
func (e *Editor) History() []string { return append([]string(nil), e.history...) }

// SetPrompt changes the prefix used by subsequent reads.
func (e *Editor) SetPrompt(prompt string) { e.prompt = prompt }

// Read edits and returns one submission.
func (e *Editor) Read(ctx context.Context) (string, error) {
	return e.ReadInitial(ctx, "")
}

// EditorState is one reusable editing session. It does not read from the
// terminal, allowing a parent event loop to route keys alongside other events.
type EditorState struct {
	editor       *Editor
	buffer       []rune
	cursor       int
	historyIndex int
	historyDraft []rune
	selected     int
	menuClosed   bool
	tabMatches   []Candidate
	tabIndex     int
}

// EditorResult reports a terminal action produced by one key.
type EditorResult struct {
	Value string
	Done  bool
	Err   error
}

// Start initializes an incremental editing session.
func (e *Editor) Start(initial string) (*EditorState, error) {
	if !validInput(initial) || len(initial) > e.maxInputBytes || runeCount(initial) > e.maxInputRunes {
		return nil, ErrInputLimit
	}
	return &EditorState{
		editor: e, buffer: []rune(initial), cursor: runeCount(initial), historyIndex: len(e.history),
	}, nil
}

// Value returns the current draft.
func (s *EditorState) Value() string { return string(s.buffer) }

// Reset replaces the draft while preserving editor history and completions.
func (s *EditorState) Reset(value string) error {
	if !validInput(value) || len(value) > s.editor.maxInputBytes || runeCount(value) > s.editor.maxInputRunes {
		return ErrInputLimit
	}
	s.buffer = []rune(value)
	s.cursor = len(s.buffer)
	s.historyIndex = len(s.editor.history)
	s.historyDraft = nil
	s.selected = 0
	s.menuClosed = false
	s.tabMatches = nil
	s.tabIndex = 0
	return nil
}

// PromptState returns the current renderer snapshot.
func (s *EditorState) PromptState() PromptState {
	matches := []Candidate(nil)
	if !s.menuClosed {
		if len(s.tabMatches) > 0 {
			matches = s.tabMatches
		} else {
			matches = s.editor.completionMatches(string(s.buffer))
		}
	}
	if len(matches) > 0 {
		s.selected = clamp(s.selected, 0, len(matches)-1)
	} else {
		s.selected = 0
	}
	return PromptState{
		Prefix: s.editor.prompt, Text: string(s.buffer), Cursor: s.cursor,
		Completions: matches, Selected: s.selected,
	}
}

// Handle applies one decoded key without reading or rendering.
func (s *EditorState) Handle(key Key) EditorResult {
	changed := false
	var err error
	switch key.Kind {
	case KeyRune:
		s.buffer, s.cursor, err = s.editor.insert(s.buffer, s.cursor, []rune{key.Rune})
		changed = err == nil
	case KeyPaste:
		paste := strings.ReplaceAll(strings.ReplaceAll(key.Text, "\r\n", "\n"), "\r", "\n")
		s.buffer, s.cursor, err = s.editor.insert(s.buffer, s.cursor, []rune(paste))
		changed = err == nil
	case KeyLeft:
		if s.cursor > 0 {
			s.cursor--
		}
	case KeyRight:
		if s.cursor < len(s.buffer) {
			s.cursor++
		}
	case KeyHome:
		s.cursor = lineStart(s.buffer, s.cursor)
	case KeyEnd:
		s.cursor = lineEnd(s.buffer, s.cursor)
	case KeyBackspace:
		if s.cursor > 0 {
			s.buffer = append(s.buffer[:s.cursor-1], s.buffer[s.cursor:]...)
			s.cursor--
			changed = true
		}
	case KeyDelete:
		if s.cursor < len(s.buffer) {
			s.buffer = append(s.buffer[:s.cursor], s.buffer[s.cursor+1:]...)
			changed = true
		}
	case KeyKillLine:
		end := lineEnd(s.buffer, s.cursor)
		if end == s.cursor && end < len(s.buffer) {
			// Readline's kill-line removes the line break when invoked at the
			// end of a non-final line.
			end++
		}
		if s.cursor < end {
			s.buffer = append(s.buffer[:s.cursor], s.buffer[end:]...)
			changed = true
		}
	case KeyEOF:
		if len(s.buffer) == 0 {
			return EditorResult{Done: true, Err: io.EOF}
		}
		if s.cursor < len(s.buffer) {
			s.buffer = append(s.buffer[:s.cursor], s.buffer[s.cursor+1:]...)
			changed = true
		}
	case KeyNewline:
		s.buffer, s.cursor, err = s.editor.insert(s.buffer, s.cursor, []rune{'\n'})
		changed = err == nil
	case KeyUp, KeyDown:
		matches := s.editor.visibleMatches(string(s.buffer), s.menuClosed, s.tabMatches)
		if len(matches) > 0 {
			if key.Kind == KeyUp {
				s.selected = (s.selected - 1 + len(matches)) % len(matches)
			} else {
				s.selected = (s.selected + 1) % len(matches)
			}
		} else if len(s.editor.history) > 0 {
			if key.Kind == KeyUp && s.historyIndex > 0 {
				if s.historyIndex == len(s.editor.history) {
					s.historyDraft = append([]rune(nil), s.buffer...)
				}
				s.historyIndex--
				s.buffer = []rune(s.editor.history[s.historyIndex])
				s.cursor = len(s.buffer)
			} else if key.Kind == KeyDown && s.historyIndex < len(s.editor.history) {
				s.historyIndex++
				if s.historyIndex == len(s.editor.history) {
					s.buffer = append([]rune(nil), s.historyDraft...)
				} else {
					s.buffer = []rune(s.editor.history[s.historyIndex])
				}
				s.cursor = len(s.buffer)
			}
		}
	case KeyTab:
		if len(s.tabMatches) == 0 {
			s.tabMatches = s.editor.completionMatches(string(s.buffer))
			s.tabIndex = s.selected
		} else {
			s.tabIndex = (s.tabIndex + 1) % len(s.tabMatches)
		}
		if len(s.tabMatches) > 0 {
			s.selected = s.tabIndex
			s.buffer = []rune(s.tabMatches[s.tabIndex].Value)
			s.cursor = len(s.buffer)
			s.menuClosed = false
		}
	case KeyEscape:
		if len(s.editor.visibleMatches(string(s.buffer), s.menuClosed, s.tabMatches)) > 0 {
			s.menuClosed = true
			s.tabMatches = nil
		} else {
			return EditorResult{Done: true, Err: ErrCanceled}
		}
	case KeyInterrupt:
		return EditorResult{Done: true, Err: ErrInterrupted}
	case KeyModeSwitch:
		return EditorResult{}
	case KeyIgnored:
		return EditorResult{}
	case KeyEnter:
		matches := s.editor.visibleMatches(string(s.buffer), s.menuClosed, s.tabMatches)
		exact := false
		for _, match := range matches {
			if string(s.buffer) == match.Value {
				exact = true
				break
			}
		}
		if len(matches) > 0 && !exact && !s.editor.hasCommandArguments(string(s.buffer)) {
			s.selected = clamp(s.selected, 0, len(matches)-1)
			s.buffer = []rune(matches[s.selected].Value + " ")
			s.cursor = len(s.buffer)
			s.menuClosed = true
			s.tabMatches = nil
			break
		}
		value := string(s.buffer)
		s.editor.AddHistory(value)
		return EditorResult{Value: value, Done: true}
	}
	if err != nil {
		return EditorResult{Done: true, Err: err}
	}
	if changed {
		s.historyIndex = len(s.editor.history)
		s.menuClosed = false
		if key.Kind != KeyTab {
			s.tabMatches = nil
		}
		s.selected = 0
	}
	return EditorResult{}
}

// ReadInitial edits one submission starting with an existing draft.
func (e *Editor) ReadInitial(ctx context.Context, initial string) (string, error) {
	state, err := e.Start(initial)
	if err != nil {
		return "", err
	}
	render := func() error {
		if e.renderer == nil {
			return nil
		}
		return e.renderer.Prompt(state.PromptState())
	}
	finish := func(value string, err error) (string, error) {
		if e.renderer != nil {
			if clearErr := e.renderer.Clear(); err == nil && clearErr != nil {
				err = clearErr
			}
		}
		return value, err
	}
	if err := render(); err != nil {
		return "", err
	}

	for {
		key, err := e.decoder.ReadKey(ctx)
		if err != nil {
			return finish("", err)
		}
		result := state.Handle(key)
		if result.Done {
			return finish(result.Value, result.Err)
		}
		if err := render(); err != nil {
			return "", err
		}
	}
}

func (e *Editor) insert(buffer []rune, cursor int, addition []rune) ([]rune, int, error) {
	for _, r := range addition {
		if unicode.IsControl(r) && r != '\n' {
			return buffer, cursor, errors.New("terminal: rejected control character")
		}
	}
	if len(buffer)+len(addition) > e.maxInputRunes {
		return buffer, cursor, ErrInputLimit
	}
	next := make([]rune, 0, len(buffer)+len(addition))
	next = append(next, buffer[:cursor]...)
	next = append(next, addition...)
	next = append(next, buffer[cursor:]...)
	if len(string(next)) > e.maxInputBytes {
		return buffer, cursor, ErrInputLimit
	}
	return next, cursor + len(addition), nil
}

func (e *Editor) completionMatches(input string) []Candidate {
	if !strings.HasPrefix(input, "/") || strings.ContainsRune(input, '\n') {
		return nil
	}
	matches := make([]Candidate, 0)
	for _, candidate := range e.completions {
		if strings.HasPrefix(candidate.Value, input) {
			matches = append(matches, candidate)
		}
	}
	return matches
}

func (e *Editor) visibleMatches(input string, closed bool, tab []Candidate) []Candidate {
	if closed {
		return nil
	}
	if len(tab) > 0 {
		return tab
	}
	return e.completionMatches(input)
}

func (e *Editor) hasCommandArguments(input string) bool {
	longest := ""
	for _, candidate := range e.completions {
		value := candidate.Value
		if len(value) <= len(longest) || !strings.HasPrefix(input, value) || len(input) == len(value) {
			continue
		}
		remainder := input[len(value):]
		first, _ := utf8.DecodeRuneInString(remainder)
		if unicode.IsSpace(first) {
			longest = value
		}
	}
	return longest != "" && strings.TrimSpace(input[len(longest):]) != ""
}

func lineStart(buffer []rune, cursor int) int {
	for cursor > 0 && buffer[cursor-1] != '\n' {
		cursor--
	}
	return cursor
}

func lineEnd(buffer []rune, cursor int) int {
	for cursor < len(buffer) && buffer[cursor] != '\n' {
		cursor++
	}
	return cursor
}

func validSingleLine(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validInput(value string) bool {
	return utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n'
	}) < 0
}
