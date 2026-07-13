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
		maxCompletionRows: defaultLiveRows - 1,
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

// ReadInitial edits one submission starting with an existing draft.
func (e *Editor) ReadInitial(ctx context.Context, initial string) (string, error) {
	if !validInput(initial) || len(initial) > e.maxInputBytes || runeCount(initial) > e.maxInputRunes {
		return "", ErrInputLimit
	}
	buffer := []rune(initial)
	cursor := len(buffer)
	historyIndex := len(e.history)
	var historyDraft []rune
	selected := 0
	menuClosed := false
	var tabMatches []Candidate
	tabIndex := 0

	render := func() error {
		if e.renderer == nil {
			return nil
		}
		matches := []Candidate(nil)
		if !menuClosed {
			if len(tabMatches) > 0 {
				matches = tabMatches
			} else {
				matches = e.completionMatches(string(buffer))
			}
		}
		if len(matches) > 0 {
			selected = clamp(selected, 0, len(matches)-1)
		} else {
			selected = 0
		}
		shown := matches
		shownSelected := selected
		if len(shown) > e.maxCompletionRows {
			start := 0
			if selected >= e.maxCompletionRows {
				start = selected - e.maxCompletionRows + 1
			}
			shown = shown[start : start+e.maxCompletionRows]
			shownSelected = selected - start
		}
		return e.renderer.Prompt(PromptState{
			Prefix: e.prompt, Text: string(buffer), Cursor: cursor,
			Completions: shown, Selected: shownSelected,
		})
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
		changed := false
		switch key.Kind {
		case KeyRune:
			buffer, cursor, err = e.insert(buffer, cursor, []rune{key.Rune})
			changed = err == nil
		case KeyPaste:
			paste := strings.ReplaceAll(strings.ReplaceAll(key.Text, "\r\n", "\n"), "\r", "\n")
			buffer, cursor, err = e.insert(buffer, cursor, []rune(paste))
			changed = err == nil
		case KeyLeft:
			if cursor > 0 {
				cursor--
			}
		case KeyRight:
			if cursor < len(buffer) {
				cursor++
			}
		case KeyHome:
			cursor = lineStart(buffer, cursor)
		case KeyEnd:
			cursor = lineEnd(buffer, cursor)
		case KeyBackspace:
			if cursor > 0 {
				buffer = append(buffer[:cursor-1], buffer[cursor:]...)
				cursor--
				changed = true
			}
		case KeyDelete:
			if cursor < len(buffer) {
				buffer = append(buffer[:cursor], buffer[cursor+1:]...)
				changed = true
			}
		case KeyEOF:
			if len(buffer) == 0 {
				return finish("", io.EOF)
			}
			if cursor < len(buffer) {
				buffer = append(buffer[:cursor], buffer[cursor+1:]...)
				changed = true
			}
		case KeyNewline:
			buffer, cursor, err = e.insert(buffer, cursor, []rune{'\n'})
			changed = err == nil
		case KeyUp, KeyDown:
			matches := e.visibleMatches(string(buffer), menuClosed, tabMatches)
			if len(matches) > 0 {
				if key.Kind == KeyUp {
					selected = (selected - 1 + len(matches)) % len(matches)
				} else {
					selected = (selected + 1) % len(matches)
				}
			} else if len(e.history) > 0 {
				if key.Kind == KeyUp && historyIndex > 0 {
					if historyIndex == len(e.history) {
						historyDraft = append([]rune(nil), buffer...)
					}
					historyIndex--
					buffer = []rune(e.history[historyIndex])
					cursor = len(buffer)
				} else if key.Kind == KeyDown && historyIndex < len(e.history) {
					historyIndex++
					if historyIndex == len(e.history) {
						buffer = append([]rune(nil), historyDraft...)
					} else {
						buffer = []rune(e.history[historyIndex])
					}
					cursor = len(buffer)
				}
			}
		case KeyTab:
			if len(tabMatches) == 0 {
				tabMatches = e.completionMatches(string(buffer))
				tabIndex = selected
			} else {
				tabIndex = (tabIndex + 1) % len(tabMatches)
			}
			if len(tabMatches) > 0 {
				selected = tabIndex
				buffer = []rune(tabMatches[tabIndex].Value)
				cursor = len(buffer)
				menuClosed = false
			}
		case KeyEscape:
			if len(e.visibleMatches(string(buffer), menuClosed, tabMatches)) > 0 {
				menuClosed = true
				tabMatches = nil
			} else {
				return finish("", ErrCanceled)
			}
		case KeyInterrupt:
			return finish("", ErrInterrupted)
		case KeyEnter:
			matches := e.visibleMatches(string(buffer), menuClosed, tabMatches)
			exact := false
			for _, match := range matches {
				if string(buffer) == match.Value {
					exact = true
					break
				}
			}
			if len(matches) > 0 && !exact && !e.hasCommandArguments(string(buffer)) {
				selected = clamp(selected, 0, len(matches)-1)
				buffer = []rune(matches[selected].Value + " ")
				cursor = len(buffer)
				menuClosed = true
				tabMatches = nil
				break
			}
			value := string(buffer)
			e.AddHistory(value)
			return finish(value, nil)
		}
		if err != nil {
			return finish("", err)
		}
		if changed {
			historyIndex = len(e.history)
			menuClosed = false
			if key.Kind != KeyTab {
				tabMatches = nil
			}
			selected = 0
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
