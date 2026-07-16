package terminal

import (
	"bytes"
	"fmt"
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

func TestLiveRendererDoesNotWriteUnchangedFrame(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80})
	frame := LiveFrame{
		StyledActivity: []StyledText{MutedText("working")},
		Prompt:         PromptState{Prefix: "$ ", Text: "draft", Cursor: 5},
	}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if output.Len() != before {
		t.Fatalf("unchanged frame wrote %d additional bytes: %q", output.Len()-before, output.String()[before:])
	}
}

func TestLiveRendererRedrawsForCursorStyleAndWidthChanges(t *testing.T) {
	var output bytes.Buffer
	columns := 80
	renderer := NewLiveRenderer(&output, RendererConfig{
		TTY: true, Color: true, Columns: columns, ColumnsFunc: func() int { return columns },
	})
	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "draft", Cursor: 5}); err != nil {
		t.Fatal(err)
	}

	before := output.Len()
	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "draft", Cursor: 4}); err != nil {
		t.Fatal(err)
	}
	if output.Len() == before {
		t.Fatal("cursor-only change did not redraw")
	}

	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "draft", Cursor: 5}); err != nil {
		t.Fatal(err)
	}
	before = output.Len()
	if err := renderer.UpdateStyled([]StyledText{MutedText("$ draft")}); err != nil {
		t.Fatal(err)
	}
	if output.Len() == before {
		t.Fatal("style-only change did not redraw")
	}

	before = output.Len()
	columns = 79
	if err := renderer.UpdateStyled([]StyledText{MutedText("$ draft")}); err != nil {
		t.Fatal(err)
	}
	if output.Len() == before {
		t.Fatal("terminal width change did not redraw")
	}
}

func TestLiveRendererDoesNotRedrawInvisibleStyleChangeWithoutColor(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: false, Columns: 80})
	if err := renderer.Update([]string{"working"}); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	if err := renderer.UpdateStyled([]StyledText{MutedText("working")}); err != nil {
		t.Fatal(err)
	}
	if output.Len() != before {
		t.Fatalf("invisible style change wrote %d additional bytes", output.Len()-before)
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

func TestLiveRendererKeepsUserStartRuleAtNormalBrightness(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 8})
	if err := renderer.CommitUserMessage("$ ", "request"); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\x1b[37m───────\x1b[0m\n\x1b[36m$\x1b[0m reques\n  t\n"; got != want {
		t.Fatalf("colored user message = %q; want %q", got, want)
	}
}

func TestLiveRendererInputBoundaryIsWhiteAndOrdinaryDividerIsDim(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 8})
	if err := renderer.CommitDivider(); err != nil {
		t.Fatal(err)
	}
	if err := renderer.CommitMessage("- ", "answer", true); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "\x1b[37m───────\x1b[0m") {
		t.Fatalf("input boundary was not white: %q", got)
	}
	if !strings.Contains(got, "\x1b[2;90m───────\x1b[0m") {
		t.Fatalf("ordinary divider was not dim grey: %q", got)
	}
}

func TestLiveRendererMutedReportStylesEveryWrappedRowAfterSanitization(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 8})
	if err := renderer.CommitStyled(MutedText("✓ read unsafe\x1b[31m text")); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"\x1b[90m✓ read u\x1b[0m",
		"\x1b[90mnsafe[31\x1b[0m",
		"\x1b[90mm text\x1b[0m",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("muted wrapped report = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "\x1b[31m") || strings.Contains(got, "\x1b[32m") || strings.Contains(got, "\x1b[36m") {
		t.Fatalf("muted report retained embedded or icon color: %q", got)
	}

	output.Reset()
	renderer = NewLiveRenderer(&output, RendererConfig{TTY: true, Color: false, Columns: 8})
	if err := renderer.CommitStyled(MutedText("✓ read report")); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(output.String(), '\x1b') {
		t.Fatalf("no-color muted report emitted ANSI: %q", output.String())
	}
}

func TestLiveRendererDecoratesIconStatuses(t *testing.T) {
	renderer := NewLiveRenderer(&bytes.Buffer{}, RendererConfig{TTY: true, Color: true, Columns: 80})
	tests := map[string]string{
		"✓ task":              "\x1b[32m✓\x1b[0m task",
		"✗ task: bad input":   "\x1b[31m✗ task: bad input\x1b[0m",
		"○ Queued tool: task": "\x1b[2m○ Queued tool: task\x1b[0m",
		"■ Interrupted: task": "\x1b[2m■ Interrupted: task\x1b[0m",
	}
	for input, want := range tests {
		if got := renderer.decorate(input); got != want {
			t.Errorf("decorate(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestQueuedPreviewKeepsWrittenLabelAtNarrowWidths(t *testing.T) {
	if got := queuedPreview("next task", 80); got != "$ next task  (○ queued)" {
		t.Fatalf("queuedPreview() = %q", got)
	}
	if got := queuedPreview("next task", 10); !strings.Contains(got, "queued") {
		t.Fatalf("narrow queued preview lost written label: %q", got)
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
	for _, want := range []string{"- working response", "─", "$ next task  (○ queued)", "⠋ $ editable"} {
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
	if got := renderer.rows[1]; !strings.Contains(got, "─ mode: build ") || !strings.HasSuffix(got, " local/test ") {
		t.Fatalf("modeline labels are not padded: %q", got)
	}
}

func TestLiveRendererAlignsRowsAboveModeline(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20, MaxRows: 6})
	if err := renderer.Frame(LiveFrame{
		MessagePrefix: "- ", Message: "response", Context: []string{"context"}, Activity: []string{"✓ activity"},
		InputLeft: "mode: build", Prompt: PromptState{Prefix: "$ ", Text: "draft", Cursor: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 5 || renderer.rows[0] != "context" || renderer.rows[1] != "✓ activity" ||
		renderer.rows[2] != "- response" || !strings.HasPrefix(renderer.rows[3], "─ mode: build ") || renderer.rows[4] != "$ draft" {
		t.Fatalf("live rows, modeline, and input are not aligned as expected: %#v", renderer.rows)
	}
	if got := output.String(); !strings.Contains(got, "\x1b[32m✓\x1b[0m activity") || !strings.Contains(got, "\x1b[32m-\x1b[0m response") {
		t.Fatalf("live rows lost semantic color: %q", got)
	}
}

func TestLiveRendererKeepsModelineThinAfterTranscriptBoundaryWasCommitted(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 40, MaxRows: 6})
	if err := renderer.Frame(LiveFrame{
		InputLeft: "mode: build", InputRight: "local/test",
		Prompt: PromptState{Prefix: "$ "}, ShowDivider: false,
	}); err != nil {
		t.Fatal(err)
	}
	if got := renderer.rows[0]; !strings.HasPrefix(got, "─ mode: build ") || !strings.Contains(got, "─") || !strings.HasSuffix(got, " local/test ") {
		t.Fatalf("settled modeline lost its thin rule: %q", got)
	}
}

func TestLiveRendererSpacesSettledResponseFromModelineImmediately(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 40, MaxRows: 6})
	if err := renderer.CommitMessage("- ", "complete response", false); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Frame(LiveFrame{
		InputLeft: "mode: build", InputRight: "local/test",
		Prompt: PromptState{Prefix: "$ "}, ShowDivider: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) < 3 || renderer.rows[0] != "" {
		t.Fatalf("settled response is not spaced from modeline: %#v", renderer.rows)
	}
	if got := renderer.rows[1]; !strings.HasPrefix(got, "─ mode: build ") {
		t.Fatalf("modeline = %q", got)
	}
	if renderer.cursorRow != 2 {
		t.Fatalf("cursor row = %d, rows=%#v", renderer.cursorRow, renderer.rows)
	}
}

func TestLiveRendererSpacesWorkingActivityFromUserMessageImmediately(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 40, MaxRows: 6})
	if err := renderer.CommitUserMessage("$ ", "test again"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Frame(LiveFrame{
		Activity: []string{"Thought: Working…"}, InputCenter: "⠋ Thinking…",
		InputLeft: "mode: build", InputRight: "local/test",
		Prompt: PromptState{Prefix: "$ "}, ShowDivider: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) < 4 || renderer.rows[0] != "" || renderer.rows[1] != "Thought: Working…" {
		t.Fatalf("working activity is not spaced from user message: %#v", renderer.rows)
	}
	if renderer.cursorRow != len(renderer.rows)-1 {
		t.Fatalf("cursor row = %d, rows=%#v", renderer.cursorRow, renderer.rows)
	}
}

func TestLiveRendererSeparatesFinalAssistantFromUserWithDimRule(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20, MaxRows: 6})
	if err := renderer.CommitUserMessage("$ ", "request"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Frame(LiveFrame{
		Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "final answer"},
		Prompt: PromptState{Prefix: "$ "},
	}); err != nil {
		t.Fatal(err)
	}
	wantRule := strings.Repeat("─", 19)
	if len(renderer.rows) < 2 || renderer.rows[0] != wantRule || renderer.rows[1] != "- final answer" {
		t.Fatalf("final assistant boundary = %#v, want rule then answer", renderer.rows)
	}
	if got := output.String(); !strings.Contains(got, "\x1b[2;90m"+wantRule+"\x1b[0m") {
		t.Fatalf("final assistant rule was not dim grey: %q", got)
	}
}

func TestLiveRendererSeparatesCommittedFinalAssistantWithDimRule(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20})
	if err := renderer.CommitStyled(MutedText("✓ summary")); err != nil {
		t.Fatal(err)
	}
	if err := renderer.CommitMessage("- ", "final answer", false); err != nil {
		t.Fatal(err)
	}
	wantRule := strings.Repeat("─", 19)
	want := "\x1b[90m✓ summary\x1b[0m\n\x1b[2;90m" + wantRule + "\x1b[0m\n\x1b[32m-\x1b[0m final answer\n"
	if got := output.String(); got != want {
		t.Fatalf("committed final assistant = %q; want %q", got, want)
	}
}

func TestLiveRendererCompositeFrameShowsTransientContentInModeline(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxRows: 6})
	err := renderer.Frame(LiveFrame{
		InputLeft: "mode: build", InputCenter: "⠋ Thinking…", InputRight: "local/test",
		Prompt: PromptState{Prefix: "$ "}, ShowDivider: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := renderer.rows[0]; !strings.HasPrefix(got, "─ mode: build ─ ⠋ Thinking… ") || !strings.HasSuffix(got, " local/test ") {
		t.Fatalf("transient content is not in modeline: %#v", renderer.rows)
	}
	if strings.Contains(renderer.rows[0], "status:") {
		t.Fatalf("modeline retained the obsolete status label: %#v", renderer.rows)
	}
}

func TestLiveRendererPromotesStreamingRowsAndCommitsOnlySuffix(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 10, MaxRows: 6})
	frame := LiveFrame{
		Stream:      &StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdef"},
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
	if len(renderer.rows) == 0 || renderer.rows[0] != "  ijklmnop" {
		t.Fatalf("unfinished streaming row is not aligned: %#v", renderer.rows)
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

func TestLiveRendererKeepsStreamingRowAtTopOfCompositeFrame(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 20, MaxRows: 6})
	frame := LiveFrame{
		Stream:      &StreamMessage{ID: "answer", Prefix: "- ", Text: "partial"},
		Context:     []string{"question context"},
		Activity:    []string{"Tool: running"},
		Prompt:      PromptState{Prefix: "$ ", Text: "draft", Cursor: 5},
		ShowDivider: true,
	}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if got, want := renderer.rows[0], "- partial"; got != want {
		t.Fatalf("highest live row = %q; want %q; rows=%#v", got, want, renderer.rows)
	}
	if context, activity := indexOf(renderer.rows, "question context"), indexOf(renderer.rows, "Tool: running"); context <= 0 || activity <= context {
		t.Fatalf("stream, context, and activity order is wrong: %#v", renderer.rows)
	}

	before := output.Len()
	frame.Stream.Text = "partial response that wraps"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	promotion := output.String()[before:]
	if !strings.Contains(promotion, "- partial response t\n") {
		t.Fatalf("top streaming row was not promoted at the live boundary: %q", promotion)
	}
	if len(renderer.rows) == 0 || renderer.rows[0] != "  hat wraps" {
		t.Fatalf("unfinished suffix is not the highest live row: %#v", renderer.rows)
	}
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func TestLiveRendererStreamingFramesAreIdempotent(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 8, MaxRows: 6})
	frame := LiveFrame{Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefghij"}, Prompt: PromptState{Prefix: "$ "}}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	pending := string(renderer.stream.pending)
	before := output.Len()
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if string(renderer.stream.pending) != pending {
		t.Fatalf("repeated cumulative frame changed pending text: %q -> %q", pending, renderer.stream.pending)
	}
	if output.Len() != before {
		t.Fatalf("repeated cumulative frame wrote %d additional bytes", output.Len()-before)
	}
}

func TestLiveRendererInputMenuExpandsToTwelveAndReportsHiddenOptions(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxRows: 2, MaxInputRows: 12})
	choices := make([]Candidate, 15)
	for i := range choices {
		choices[i] = Candidate{Value: fmt.Sprintf("option-%02d", i+1)}
	}
	if err := renderer.Prompt(PromptState{Prefix: "select> ", Completions: choices, Selected: 14}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 12 {
		t.Fatalf("menu rows = %d, want 12: %#v", len(renderer.rows), renderer.rows)
	}
	joined := strings.Join(renderer.rows, "\n")
	if !strings.Contains(joined, "> option-15") || !strings.Contains(joined, "Showing 6-15 of 15 options; 5 hidden") {
		t.Fatalf("menu viewport or overflow missing: %#v", renderer.rows)
	}

	if err := renderer.Prompt(PromptState{Prefix: "select> ", Text: "option-15", Cursor: 9}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 1 || renderer.rows[0] != "select> option-15" {
		t.Fatalf("collapsed input = %#v", renderer.rows)
	}
}

func TestLiveRendererKeepsLiveBudgetSeparateFromInputMenu(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxRows: 2, MaxInputRows: 5})
	err := renderer.Frame(LiveFrame{
		Context: []string{"live one", "live two"},
		Prompt:  PromptState{Prefix: "pick> ", Completions: []Candidate{{Value: "one"}, {Value: "two"}, {Value: "three"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(renderer.rows, "\n"); !strings.Contains(got, "live one\nlive two\npick> \n> one\n  two\n  three") {
		t.Fatalf("live and input regions were not independently budgeted: %#v", renderer.rows)
	}
}

func TestLiveRendererDividerDoesNotReduceTwelveRowMenu(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxRows: 1, MaxInputRows: 12})
	choices := make([]Candidate, 15)
	for i := range choices {
		choices[i] = Candidate{Value: fmt.Sprintf("choice-%02d", i+1)}
	}
	if err := renderer.Frame(LiveFrame{
		Context: []string{"live"}, ShowDivider: true,
		Prompt: PromptState{Prefix: "pick> ", Completions: choices},
	}); err != nil {
		t.Fatal(err)
	}
	// One live row + one divider + twelve independently budgeted input rows.
	if len(renderer.rows) != 14 {
		t.Fatalf("frame rows = %d, want 14: %#v", len(renderer.rows), renderer.rows)
	}
	if got := strings.Join(renderer.rows, "\n"); !strings.Contains(got, "Showing 1-10 of 15 options; 5 hidden") {
		t.Fatalf("full input viewport missing: %#v", renderer.rows)
	}
}

func TestLiveRendererOneChoiceRowStillReportsHiddenOptions(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxInputRows: 2})
	if err := renderer.Prompt(PromptState{
		Prefix: "pick> ", Completions: []Candidate{{Value: "one"}, {Value: "two"}, {Value: "three"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || !strings.Contains(renderer.rows[1], "2 options hidden") {
		t.Fatalf("single choice row did not report overflow: %#v", renderer.rows)
	}
}

func TestLiveRendererNarrowDividerDoesNotWrap(t *testing.T) {
	for _, columns := range []int{1, 2} {
		var output bytes.Buffer
		renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: columns})
		if err := renderer.Frame(LiveFrame{ShowDivider: true, Prompt: PromptState{Prefix: "$ "}}); err != nil {
			t.Fatal(err)
		}
		if got := physicalRowCount(renderer.rows[:1], columns); got != 1 {
			t.Fatalf("columns=%d divider physical rows=%d: %#v", columns, got, renderer.rows)
		}
	}
}

func TestLiveRendererSpacesBlockAndCompactCommits(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{Columns: 80})
	for _, text := range []string{"tool one", "tool two"} {
		if err := renderer.Commit(text); err != nil {
			t.Fatal(err)
		}
	}
	if err := renderer.CommitBlock("edit\n-old\n+new\n"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Commit("tool three"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.CommitBlock("assistant"); err != nil {
		t.Fatal(err)
	}
	want := "tool one\ntool two\n\nedit\n-old\n+new\n\ntool three\n\nassistant\n"
	if got := output.String(); got != want {
		t.Fatalf("commit spacing = %q; want %q", got, want)
	}
}

func TestLiveRendererSpacesStreamOnceAfterCompactCommit(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 10, MaxRows: 6})
	if err := renderer.CommitStyled(MutedText("✓ summary")); err != nil {
		t.Fatal(err)
	}
	frame := LiveFrame{Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "abc"}, Prompt: PromptState{Prefix: "$ "}}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	wantRule := strings.Repeat("─", 9)
	if len(renderer.rows) < 2 || renderer.rows[0] != wantRule || renderer.rows[1] != "- abc" {
		t.Fatalf("live stream is not separated from compact summary: %#v", renderer.rows)
	}
	frame.Stream.Text = "abcdefghijklmnop"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile("\\x1b(?:\\[\\?25[lh]|\\[2K|\\[[0-9]+[AB])").ReplaceAllString(output.String(), "")
	plain = strings.ReplaceAll(plain, "\r", "")
	if got := strings.Count(plain, "\n"+wantRule+"\n- abcdefgh\n"); got != 1 {
		t.Fatalf("stream block separator count = %d; output=%q", got, output.String())
	}
	if err := renderer.CommitStream(StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefghijklmnop!"}, false); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Commit("next tool"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(output.String(), "\nnext tool\n") {
		t.Fatalf("commit after stream was not block-separated: %q", output.String())
	}
}
