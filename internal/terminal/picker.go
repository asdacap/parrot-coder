package terminal

import (
	"context"
	"io"
	"strings"
)

type pickerConfig struct {
	prompt              string
	maxInputBytes       int
	maxInputRunes       int
	maxRows             int
	renderer            *LiveRenderer
	disableAutoRenderer bool
}

// PickerOption customizes a Picker.
type PickerOption func(*pickerConfig)

func WithPickerPrompt(prompt string) PickerOption {
	return func(config *pickerConfig) { config.prompt = prompt }
}

// WithPickerLimits sets filter byte/rune and visible option bounds.
func WithPickerLimits(bytes, runes, rows int) PickerOption {
	return func(config *pickerConfig) {
		if bytes > 0 {
			config.maxInputBytes = bytes
		}
		if runes > 0 {
			config.maxInputRunes = runes
		}
		if rows > 0 {
			config.maxRows = rows
		}
	}
}

// WithPickerRenderer uses renderer for redraws. Passing nil disables them.
func WithPickerRenderer(renderer *LiveRenderer) PickerOption {
	return func(config *pickerConfig) {
		config.renderer = renderer
		config.disableAutoRenderer = renderer == nil
	}
}

// Picker filters and selects from a fixed set of options.
type Picker struct {
	decoder       *KeyDecoder
	renderer      *LiveRenderer
	options       []Candidate
	prompt        string
	maxInputBytes int
	maxInputRunes int
	maxRows       int
}

func NewPicker(rw io.ReadWriter, options []Candidate, config ...PickerOption) *Picker {
	return NewPickerIO(rw, rw, options, config...)
}

func NewPickerIO(reader io.Reader, writer io.Writer, options []Candidate, settings ...PickerOption) *Picker {
	return newPicker(NewKeyDecoder(reader), writer, options, settings...)
}

// NewPickerDecoder creates a picker that shares a key decoder with an editor.
func NewPickerDecoder(decoder *KeyDecoder, writer io.Writer, options []Candidate, settings ...PickerOption) *Picker {
	return newPicker(decoder, writer, options, settings...)
}

func newPicker(decoder *KeyDecoder, writer io.Writer, options []Candidate, settings ...PickerOption) *Picker {
	config := pickerConfig{
		prompt:        "Filter: ",
		maxInputBytes: 4 << 10,
		maxInputRunes: 1024,
		maxRows:       defaultLiveRows - 1,
	}
	for _, setting := range settings {
		setting(&config)
	}
	if config.renderer == nil && !config.disableAutoRenderer && writer != nil {
		config.renderer = NewLiveRenderer(writer, RendererConfig{TTY: IsTTY(writer)})
	}
	valid := make([]Candidate, 0, len(options))
	for _, option := range options {
		if validSingleLine(option.Value) && validSingleLine(option.Description) {
			valid = append(valid, option)
		}
	}
	return &Picker{
		decoder:       decoder,
		renderer:      config.renderer,
		options:       valid,
		prompt:        config.prompt,
		maxInputBytes: config.maxInputBytes,
		maxInputRunes: config.maxInputRunes,
		maxRows:       config.maxRows,
	}
}

// Pick reads until an option is selected or the operation is canceled.
func (p *Picker) Pick(ctx context.Context) (Candidate, error) {
	var query []rune
	cursor := 0
	selected := 0

	finish := func(value Candidate, err error) (Candidate, error) {
		if p.renderer != nil {
			if clearErr := p.renderer.Clear(); err == nil && clearErr != nil {
				err = clearErr
			}
		}
		return value, err
	}
	render := func() error {
		if p.renderer == nil {
			return nil
		}
		matches := p.matches(string(query))
		shown := matches
		if len(shown) > p.maxRows {
			start := 0
			if selected >= p.maxRows {
				start = selected - p.maxRows + 1
			}
			shown = shown[start : start+p.maxRows]
		}
		shownSelected := selected
		if len(matches) > p.maxRows && selected >= p.maxRows {
			shownSelected = p.maxRows - 1
		}
		return p.renderer.Prompt(PromptState{
			Prefix: p.prompt, Text: string(query), Cursor: cursor,
			Completions: shown, Selected: shownSelected,
		})
	}
	if err := render(); err != nil {
		return Candidate{}, err
	}

	for {
		key, err := p.decoder.ReadKey(ctx)
		if err != nil {
			return finish(Candidate{}, err)
		}
		switch key.Kind {
		case KeyRune:
			query, cursor, err = pickerInsert(query, cursor, []rune{key.Rune}, p.maxInputBytes, p.maxInputRunes)
			selected = 0
		case KeyPaste:
			if strings.ContainsAny(key.Text, "\r\n") {
				err = ErrInputLimit
			} else {
				query, cursor, err = pickerInsert(query, cursor, []rune(key.Text), p.maxInputBytes, p.maxInputRunes)
				selected = 0
			}
		case KeyLeft:
			if cursor > 0 {
				cursor--
			}
		case KeyRight:
			if cursor < len(query) {
				cursor++
			}
		case KeyHome:
			cursor = 0
		case KeyEnd:
			cursor = len(query)
		case KeyBackspace:
			if cursor > 0 {
				query = append(query[:cursor-1], query[cursor:]...)
				cursor--
				selected = 0
			}
		case KeyDelete:
			if cursor < len(query) {
				query = append(query[:cursor], query[cursor+1:]...)
				selected = 0
			}
		case KeyEOF:
			if len(query) == 0 {
				return finish(Candidate{}, io.EOF)
			}
			if cursor < len(query) {
				query = append(query[:cursor], query[cursor+1:]...)
			}
		case KeyUp, KeyDown, KeyTab:
			matches := p.matches(string(query))
			if len(matches) > 0 {
				if key.Kind == KeyUp {
					selected = (selected - 1 + len(matches)) % len(matches)
				} else {
					selected = (selected + 1) % len(matches)
				}
			}
		case KeyEnter:
			matches := p.matches(string(query))
			if len(matches) > 0 {
				return finish(matches[clamp(selected, 0, len(matches)-1)], nil)
			}
		case KeyEscape:
			return finish(Candidate{}, ErrCanceled)
		case KeyInterrupt:
			return finish(Candidate{}, ErrInterrupted)
		case KeyNewline:
			// A picker filter is deliberately single-line.
		}
		if err != nil {
			return finish(Candidate{}, err)
		}
		matches := p.matches(string(query))
		if len(matches) == 0 {
			selected = 0
		} else {
			selected = clamp(selected, 0, len(matches)-1)
		}
		if err := render(); err != nil {
			return Candidate{}, err
		}
	}
}

func (p *Picker) matches(query string) []Candidate {
	needle := strings.ToLower(query)
	matches := make([]Candidate, 0, len(p.options))
	for _, option := range p.options {
		if needle == "" || strings.Contains(strings.ToLower(option.Value), needle) ||
			strings.Contains(strings.ToLower(option.Description), needle) {
			matches = append(matches, option)
		}
	}
	return matches
}

func pickerInsert(buffer []rune, cursor int, addition []rune, maxBytes, maxRunes int) ([]rune, int, error) {
	if len(buffer)+len(addition) > maxRunes {
		return buffer, cursor, ErrInputLimit
	}
	next := make([]rune, 0, len(buffer)+len(addition))
	next = append(next, buffer[:cursor]...)
	next = append(next, addition...)
	next = append(next, buffer[cursor:]...)
	if len(string(next)) > maxBytes {
		return buffer, cursor, ErrInputLimit
	}
	return next, cursor + len(addition), nil
}
