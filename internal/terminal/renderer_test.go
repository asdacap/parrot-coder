package terminal

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestLiveRendererExactBytesAndClear(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80})
	if err := renderer.Update([]string{"one"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Clear(); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[?25l\x1b[2Kone\x1b[?25h" +
		"\x1b[?25l\r\x1b[2K\r\x1b[?25h"
	if output.String() != want {
		t.Fatalf("renderer bytes = %q; want %q", output.String(), want)
	}
}

func TestLiveRendererAllowedEscapesAndSanitization(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 4, MaxRows: 3})
	if err := renderer.Update([]string{"abcdef", "bad\x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Update([]string{"next"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Commit("safe\x1b]0;title\a"); err != nil {
		t.Fatal(err)
	}

	data := output.String()
	escape := regexp.MustCompile("\\x1b(?:\\[\\?25[lh]|\\[2K|\\[[0-9]+[AB])")
	withoutAllowed := escape.ReplaceAllString(data, "")
	if strings.ContainsRune(withoutAllowed, '\x1b') {
		t.Fatalf("renderer emitted a disallowed escape: %q", data)
	}
	if strings.Contains(data, "\x1b[2J") || strings.Contains(data, "\x1b]") {
		t.Fatalf("untrusted terminal control survived: %q", data)
	}
	if !strings.HasSuffix(data, "safe]0;title\n") {
		t.Fatalf("Commit output = %q", data)
	}
}

func TestLiveRendererWidthResizeAndRowBound(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 4, MaxRows: 2})
	if err := renderer.Update([]string{"123456789"}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || renderer.rows[0] != "5678" || renderer.rows[1] != "9" {
		t.Fatalf("wrapped rows = %#v", renderer.rows)
	}
	if err := renderer.Clear(); err != nil {
		t.Fatal(err)
	}
	renderer.SetColumns(2)
	if err := renderer.Update([]string{"界a"}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || renderer.rows[0] != "界" || renderer.rows[1] != "a" {
		t.Fatalf("resized rows = %#v", renderer.rows)
	}
}

func TestLiveRendererPlainFallbackDeduplicates(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: false, Columns: 80})
	if err := renderer.Update([]string{"working", "working", "unsafe\x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Update([]string{"working", "done"}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "working\nunsafe[2J\ndone\n"; got != want {
		t.Fatalf("plain output = %q; want %q", got, want)
	}
	if strings.ContainsRune(output.String(), '\x1b') {
		t.Fatal("plain renderer emitted an escape")
	}
}

func TestLiveRendererColoredMessagesAlignAndSanitize(t *testing.T) {
	var output bytes.Buffer
	columns := 8
	renderer := NewLiveRenderer(&output, RendererConfig{
		TTY: true, Color: true, Columns: columns,
	})
	if err := renderer.CommitMessage("$ ", "abcdefgh\nunsafe\x1b[2J", true); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[36m$\x1b[0m abcdef\n  gh\n  unsafe\n  [2J\n\x1b[2;90m───────\x1b[0m\n"
	if output.String() != want {
		t.Fatalf("colored message = %q; want %q", output.String(), want)
	}
	if strings.Contains(output.String(), "\x1b[2J") {
		t.Fatal("untrusted escape survived colored rendering")
	}
}

func TestLiveRendererPromptUsesHangingIndentAndExplicitResize(t *testing.T) {
	var output bytes.Buffer
	columns := 8
	renderer := NewLiveRenderer(&output, RendererConfig{
		TTY: true, Columns: columns,
	})
	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "abcdefgh", Cursor: 8}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || renderer.rows[0] != "$ abcdef" || renderer.rows[1] != "  gh" {
		t.Fatalf("prompt rows = %#v", renderer.rows)
	}
	if err := renderer.Clear(); err != nil {
		t.Fatal(err)
	}
	columns = 5
	renderer.SetColumns(columns)
	if err := renderer.UpdateMessage("- ", "abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || renderer.rows[0] != "- abc" || renderer.rows[1] != "  def" {
		t.Fatalf("resized message rows = %#v", renderer.rows)
	}
}

func TestLiveRendererClearsRowsReflowedByTerminalShrink(t *testing.T) {
	var output bytes.Buffer
	columns := 8
	renderer := NewLiveRenderer(&output, RendererConfig{
		TTY: true, Columns: columns, ColumnsFunc: func() int { return columns },
	})
	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "abcdefgh", Cursor: 8}); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	columns = 4
	if err := renderer.UpdateMessage("- ", "ok"); err != nil {
		t.Fatal(err)
	}
	redraw := output.String()[before:]
	if !strings.Contains(redraw, "\x1b[2A") || strings.Count(redraw, "\x1b[2K") != 3 {
		t.Fatalf("shrink redraw did not clear three reflowed rows: %q", redraw)
	}
}

func TestLiveRendererCompositeFrameKeepsBusyEditorVisible(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 40, MaxRows: 6})
	err := renderer.Frame(LiveFrame{
		MessagePrefix: "- ", Message: "working response", Pending: []string{"next task"},
		Prompt: PromptState{Prefix: "$ ", Text: "editable", Cursor: 8},
		Busy:   true, Spinner: "⠋", ShowDivider: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(renderer.rows, "\n")
	for _, want := range []string{"- working response", "─", "$ next task  (queued)", "⠋ $ editable"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frame missing %q: %#v", want, renderer.rows)
		}
	}
	if renderer.cursorRow != len(renderer.rows)-1 {
		t.Fatalf("cursor row = %d, rows=%#v", renderer.cursorRow, renderer.rows)
	}
}

func TestLiveRendererCompositeFrameShowsInputStatusBar(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 40, MaxRows: 6})
	err := renderer.Frame(LiveFrame{
		MessagePrefix: "- ", Message: "working response",
		InputLeft: "mode: build", InputRight: "local/test",
		Prompt:      PromptState{Prefix: "$ ", Text: "editable", Cursor: 8},
		ShowDivider: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(renderer.rows, "\n")
	for _, want := range []string{"mode: build", "local/test", "$ editable"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frame missing %q: %#v", want, renderer.rows)
		}
	}
	if renderer.cursorRow != len(renderer.rows)-1 {
		t.Fatalf("cursor row = %d, rows=%#v", renderer.cursorRow, renderer.rows)
	}
	if got := renderer.rows[1]; !strings.Contains(got, "─ mode: build ") || !strings.Contains(got, " local/test ") {
		t.Fatalf("modeline labels are not padded: %q", got)
	}
}

func TestLiveRendererCompositeFrameShowsStatusInModeline(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxRows: 6})
	err := renderer.Frame(LiveFrame{
		InputLeft: "mode: build", Status: "tool running", InputRight: "local/test",
		Prompt: PromptState{Prefix: "$ "}, ShowDivider: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(renderer.rows, "\n")
	if !strings.Contains(joined, "─ mode: build ─ status: tool running ─ local/test ") {
		t.Fatalf("status is not in modeline: %#v", renderer.rows)
	}
	if strings.Contains(joined, "\nstatus: tool running\n") {
		t.Fatalf("status was also rendered as an activity row: %#v", renderer.rows)
	}
}

func TestLiveRendererPromotesStreamingRowsAndCommitsOnlySuffix(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 10, MaxRows: 6})
	frame := LiveFrame{
		Stream:      &StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefgh"},
		Prompt:      PromptState{Prefix: "$ ", Text: "draft", Cursor: 5},
		ShowDivider: true,
	}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if renderer.stream.started {
		t.Fatal("short unfinished row was committed")
	}
	before := output.Len()
	frame.Stream.Text = "abcdefghijklmnop"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	promotion := output.String()[before:]
	if !strings.Contains(promotion, "- abcdefgh\n") {
		t.Fatalf("stable row was not promoted to scrollback: %q", promotion)
	}
	if !renderer.stream.started || string(renderer.stream.pending) != "ijklmnop" {
		t.Fatalf("stream state = %#v", renderer.stream)
	}
	if renderer.cursorRow != len(renderer.rows)-1 || !strings.Contains(strings.Join(renderer.rows, "\n"), "$ draft") {
		t.Fatalf("editor was not restored: rows=%#v cursor=%d", renderer.rows, renderer.cursorRow)
	}

	before = output.Len()
	if err := renderer.CommitStream(StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefghijklmnop!"}, true); err != nil {
		t.Fatal(err)
	}
	completion := output.String()[before:]
	if strings.Contains(completion, "abcdefgh") || !strings.Contains(completion, "  ijklmnop") || !strings.Contains(completion, "!") {
		t.Fatalf("completion duplicated prefix or lost suffix: %q", completion)
	}
	if renderer.stream.id != "" {
		t.Fatalf("completed stream was retained: %#v", renderer.stream)
	}
}

func TestLiveRendererStreamingFramesAreIdempotent(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 8, MaxRows: 6})
	frame := LiveFrame{Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefghij"}, Prompt: PromptState{Prefix: "$ "}}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	pending := string(renderer.stream.pending)
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if string(renderer.stream.pending) != pending {
		t.Fatalf("repeated cumulative frame changed pending text: %q -> %q", pending, renderer.stream.pending)
	}
}
