package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func readEditor(t *testing.T, input string, options ...EditorOption) (string, error) {
	t.Helper()
	editor := NewEditorIO(bytes.NewBufferString(input), nil, options...)
	return editor.Read(context.Background())
}

func TestEditorEditingAndCursor(t *testing.T) {
	value, err := readEditor(t, "abc\x1b[D\x1b[D\x1b[3~\r")
	if err != nil || value != "ac" {
		t.Fatalf("Read() = %q, %v; want ac", value, err)
	}

	value, err = readEditor(t, "aé界\x1b[D\x7fX\r")
	if err != nil || value != "aX界" {
		t.Fatalf("UTF-8 Read() = %q, %v; want aX界", value, err)
	}

	value, err = readEditor(t, "first\x0asecond\x1b[H>\x1b[F<\r")
	if err != nil || value != "first\n>second<" {
		t.Fatalf("multiline Read() = %q, %v", value, err)
	}

	value, err = readEditor(t, "first\x0asecond\x01>\x05<\r")
	if err != nil || value != "first\n>second<" {
		t.Fatalf("Ctrl-A/Ctrl-E Read() = %q, %v", value, err)
	}

	value, err = readEditor(t, "before\x01\x0bcleared\r")
	if err != nil || value != "cleared" {
		t.Fatalf("Ctrl-K Read() = %q, %v; want cleared", value, err)
	}

	value, err = readEditor(t, "first\nsecond\x01\x1b[C\x0b\r")
	if err != nil || value != "first\ns" {
		t.Fatalf("multiline Ctrl-K Read() = %q, %v; want first\\ns", value, err)
	}

	value, err = readEditor(t, "first\nsecond\x01\x1b[D\x0b\r")
	if err != nil || value != "firstsecond" {
		t.Fatalf("end-of-line Ctrl-K Read() = %q, %v; want firstsecond", value, err)
	}
}

func TestEditorHistory(t *testing.T) {
	editor := NewEditorIO(bytes.NewBufferString("\x1b[A\rrepeat\r"), nil,
		WithEditorHistory([]string{"older", "newer", "newer"}))
	value, err := editor.Read(context.Background())
	if err != nil || value != "newer" {
		t.Fatalf("first Read() = %q, %v", value, err)
	}
	value, err = editor.Read(context.Background())
	if err != nil || value != "repeat" {
		t.Fatalf("second Read() = %q, %v", value, err)
	}
	want := []string{"older", "newer", "repeat"}
	if got := editor.History(); !equalStrings(got, want) {
		t.Fatalf("History() = %#v; want %#v", got, want)
	}
}

func TestEditorCompletionFilteringNavigationAndNestedNames(t *testing.T) {
	candidates := []Candidate{
		{Value: "/zebra"},
		{Value: "/config model", Description: "choose a model"},
		{Value: "/config mode"},
	}
	value, err := readEditor(t, "/config mo\x1b[B\rvalue\r", WithCompletions(candidates))
	if err != nil || value != "/config model value" {
		t.Fatalf("Read() = %q, %v", value, err)
	}

	candidates = []Candidate{{Value: "/apple"}, {Value: "/apply"}}
	value, err = readEditor(t, "/a\t\t\r", WithCompletions(candidates))
	if err != nil || value != "/apply" {
		t.Fatalf("tab Read() = %q, %v", value, err)
	}
}

func TestEditorExactCompletionSubmitsAndInitialDraftIsEditable(t *testing.T) {
	value, err := readEditor(t, "/help\r", WithCompletions([]Candidate{{Value: "/help", Description: "help"}, {Value: "/helper"}}))
	if err != nil || value != "/help" {
		t.Fatalf("exact completion Read() = %q, %v", value, err)
	}

	editor := NewEditorIO(bytes.NewBufferString("!\r"), nil)
	value, err = editor.ReadInitial(context.Background(), "draft")
	if err != nil || value != "draft!" {
		t.Fatalf("ReadInitial() = %q, %v", value, err)
	}
}

func TestEditorBracketedPasteAndSignals(t *testing.T) {
	value, err := readEditor(t, "x\x1b[200~α\r\nbeta\x1b[201~\r")
	if err != nil || value != "xα\nbeta" {
		t.Fatalf("paste Read() = %q, %v", value, err)
	}

	_, err = readEditor(t, "\x03")
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Ctrl-C error = %v", err)
	}
	_, err = readEditor(t, "\x04")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty Ctrl-D error = %v", err)
	}
	value, err = readEditor(t, "ab\x1b[D\x04\r")
	if err != nil || value != "a" {
		t.Fatalf("nonempty Ctrl-D Read() = %q, %v", value, err)
	}
	_, err = readEditor(t, "\x1b")
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Escape error = %v", err)
	}
}

func TestEditorBoundsAndInvalidInput(t *testing.T) {
	_, err := readEditor(t, "abc", WithEditorLimits(2, 10, 10, 2))
	if !errors.Is(err, ErrInputLimit) {
		t.Fatalf("byte bound error = %v", err)
	}
	value, err := readEditor(t, "\xffok\r")
	if err != nil || value != "ok" {
		t.Fatalf("invalid UTF-8 terminated editor: value=%q err=%v", value, err)
	}
	value, err = readEditor(t, "\x1b[200~bad\tdata\x1b[201~ok\r")
	if err != nil || value != "ok" {
		t.Fatalf("invalid paste terminated editor: value=%q err=%v", value, err)
	}
}

func TestKeyDecoderIgnoresUnboundControls(t *testing.T) {
	bound := map[byte]bool{0x01: true, 0x03: true, 0x04: true, 0x05: true, 0x08: true, 0x09: true, 0x0a: true, 0x0b: true, 0x0d: true, 0x18: true, 0x1b: true, 0x1e: true}
	for b := byte(0); b < 0x20; b++ {
		if bound[b] {
			continue
		}
		key, err := NewKeyDecoder(bytes.NewReader([]byte{b})).ReadKey(context.Background())
		if err != nil || key.Kind != KeyIgnored {
			t.Fatalf("control byte 0x%02x: key=%#v err=%v", b, key, err)
		}
	}
}

func TestKeyDecoderReadlineBindings(t *testing.T) {
	for _, test := range []struct {
		input byte
		want  KeyKind
	}{
		{input: 0x01, want: KeyHome},     // Ctrl-A
		{input: 0x05, want: KeyEnd},      // Ctrl-E
		{input: 0x0b, want: KeyKillLine}, // Ctrl-K
	} {
		key, err := NewKeyDecoder(bytes.NewReader([]byte{test.input})).ReadKey(context.Background())
		if err != nil || key.Kind != test.want {
			t.Fatalf("control byte 0x%02x: key=%#v err=%v; want kind %v", test.input, key, err, test.want)
		}
	}
}

func TestEditorIgnoresUnknownFunctionAndEscapeSequences(t *testing.T) {
	for _, input := range []string{"before\x1bOPafter\r", "before\x1b[5~after\r", "before\x1bxafter\r"} {
		value, err := readEditor(t, input)
		if err != nil || value != "beforeafter" {
			t.Fatalf("input %q: value=%q err=%v", input, value, err)
		}
	}
}

func TestKeyDecoderContextReaderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decoder := NewKeyDecoder(contextOnlyReader{})
	_, err := decoder.ReadKey(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadKey() error = %v", err)
	}
}

func TestKeyDecoderIgnoresTimedTTYEOF(t *testing.T) {
	decoder := NewKeyDecoder(&idleEOFReader{})
	decoder.timedTTY = true
	key, err := decoder.ReadKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if key.Kind != KeyRune || key.Rune != 'x' {
		t.Fatalf("key = %#v", key)
	}
}

func TestKeyDecoderModeSwitch(t *testing.T) {
	for _, input := range []string{"\x18", "\x1e", "\x1b[Z"} {
		decoder := NewKeyDecoder(bytes.NewBufferString(input))
		key, err := decoder.ReadKey(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if key.Kind != KeyModeSwitch {
			t.Fatalf("input %q: key kind = %v, want %v", input, key.Kind, KeyModeSwitch)
		}
	}
}

func TestIncrementalEditorStateMatchesBlockingEditing(t *testing.T) {
	editor := NewEditorIO(bytes.NewBuffer(nil), nil, WithCompletions([]Candidate{{Value: "/status", Description: "state"}}))
	state, err := editor.Start("ab")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{{Kind: KeyLeft}, {Kind: KeyRune, Rune: 'X'}} {
		if result := state.Handle(key); result.Done || result.Err != nil {
			t.Fatalf("Handle(%#v) = %#v", key, result)
		}
	}
	if state.Value() != "aXb" {
		t.Fatalf("draft = %q", state.Value())
	}
	result := state.Handle(Key{Kind: KeyEnter})
	if !result.Done || result.Err != nil || result.Value != "aXb" {
		t.Fatalf("submit = %#v", result)
	}
	if got := editor.History(); len(got) != 1 || got[0] != "aXb" {
		t.Fatalf("history = %#v", got)
	}
	if err := state.Reset("/st"); err != nil {
		t.Fatal(err)
	}
	prompt := state.PromptState()
	if len(prompt.Completions) != 1 || prompt.Completions[0].Value != "/status" {
		t.Fatalf("completions = %#v", prompt.Completions)
	}
}

func TestEditorPromptStateLeavesMenuViewportToRenderer(t *testing.T) {
	choices := make([]Candidate, 15)
	for i := range choices {
		choices[i] = Candidate{Value: fmt.Sprintf("/command-%02d", i+1)}
	}
	editor := NewEditorIO(bytes.NewBuffer(nil), nil, WithCompletions(choices), WithEditorLimits(1024, 1024, 10, 2))
	state, err := editor.Start("/")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(state.PromptState().Completions); got != 15 {
		t.Fatalf("PromptState prematurely sliced completions to %d", got)
	}
}

type contextOnlyReader struct{}

func (contextOnlyReader) Read([]byte) (int, error) { return 0, errors.New("unexpected Read") }
func (contextOnlyReader) ReadContext(ctx context.Context, _ []byte) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

type idleEOFReader struct{ reads int }

func (r *idleEOFReader) Read(buffer []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return 0, io.EOF
	}
	buffer[0] = 'x'
	return 1, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
