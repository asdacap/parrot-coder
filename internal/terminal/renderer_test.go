package terminal

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
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

func TestLiveRendererColorsLiveSurface(t *testing.T) {
	tests := []struct {
		name  string
		color bool
		rows  []string
		want  string
	}{
		{
			name:  "ordinary and empty rows",
			color: true,
			rows:  []string{"x", ""},
			want: "\x1b[?25l" +
				"\x1b[48;5;236m\x1b[2K\x1b[0m\x1b[38;5;252;48;5;236mx\x1b[0m\r\n" +
				"\x1b[48;5;236m\x1b[2K\x1b[0m" +
				"\x1b[?25h",
		},
		{
			name:  "semantic foreground",
			color: true,
			rows:  []string{"✓ ok"},
			want: "\x1b[?25l\x1b[48;5;236m\x1b[2K\x1b[0m" +
				"\x1b[32;48;5;236m✓\x1b[0m\x1b[38;5;252;48;5;236m ok\x1b[0m\x1b[?25h",
		},
		{
			name:  "color disabled",
			color: false,
			rows:  []string{"x"},
			want:  "\x1b[?25l\x1b[2Kx\x1b[?25h",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: test.color, Columns: 4})
			if err := renderer.Update(test.rows); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != test.want {
				t.Fatalf("renderer bytes = %q; want %q", got, test.want)
			}
		})
	}
}

func TestLiveRendererOrdersTaskFramesPostOrder(t *testing.T) {
	tests := []struct {
		name   string
		frames []LiveFrame
		want   string
		class  string
	}{
		{
			name: "nested children before statuses",
			frames: []LiveFrame{
				{TaskID: "root", SessionID: "session-root", MainStatus: true, StyledActivity: []StyledText{{Text: "root status"}}, Prompt: PromptState{Prefix: "$ "}},
				{TaskID: "child", SessionID: "session-child", ParentSessionID: "session-root", MainStatus: true, StyledActivity: []StyledText{{Text: "child status"}}},
				{TaskID: "grandchild", SessionID: "session-grandchild", ParentSessionID: "session-child", StyledActivity: []StyledText{{Text: "grandchild work"}}},
			},
			want: "grandchild work\nchild status\nroot status\n$ \n",
		},
		{
			name: "orphan remains visible",
			frames: []LiveFrame{
				{TaskID: "orphan", SessionID: "session-orphan", ParentSessionID: "session-missing", StyledActivity: []StyledText{{Text: "orphan work"}}},
				{TaskID: "root", SessionID: "session-root", MainStatus: true, Prompt: PromptState{Prefix: "$ "}},
			},
			want: "orphan work\n$ \n",
		},
		{
			name: "cycle rejected",
			frames: []LiveFrame{
				{TaskID: "one", SessionID: "session-one", ParentSessionID: "session-two"},
				{TaskID: "two", SessionID: "session-two", ParentSessionID: "session-one"},
			},
			class: "frame_task_cycle",
		},
		{
			name: "duplicate status rejected",
			frames: []LiveFrame{
				{TaskID: "root", SessionID: "session-root", MainStatus: true},
				{TaskID: "root", SessionID: "session-root", MainStatus: true},
			},
			class: "frame_status_conflict",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := NewLiveRenderer(&output, RendererConfig{Columns: 80, MaxRows: 20})
			err := renderer.Frames(test.frames)
			if class := RenderErrorClass(err); class != test.class {
				t.Fatalf("error class = %q, want %q (err=%v)", class, test.class, err)
			}
			if test.class == "" && output.String() != test.want {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
		})
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

func TestLiveRendererStylesUserMessageAfterEmptyLine(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 8})
	if err := renderer.CommitUserMessage("$ ", "request"); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\n\x1b[32m$ reques\x1b[0m\n\x1b[32m  t\x1b[0m\n"; got != want {
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

func TestPendingPreviewKeepsWrittenLabelAtNarrowWidths(t *testing.T) {
	if got := pendingPreview("next task", 80); got != "$ next task  (○ pending)" {
		t.Fatalf("pendingPreview() = %q", got)
	}
	if got := pendingPreview("next task", 10); !strings.Contains(got, "pending") {
		t.Fatalf("narrow pending preview lost written label: %q", got)
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
	if len(renderer.rows) != 3 || renderer.rows[0] != "" || renderer.rows[1] != "- abc" || renderer.rows[2] != "  def" {
		t.Fatalf("resized message rows = %#v", renderer.rows)
	}
}

func TestLiveRendererPromptRendersUserInputGreen(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 8})
	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "abcdefgh", Cursor: 8}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\x1b[36;48;5;236m$\x1b[0m\x1b[38;5;252;48;5;236m \x1b[0m\x1b[32;48;5;236mabcdef\x1b[0m",
		"\x1b[38;5;252;48;5;236m  \x1b[0m\x1b[32;48;5;236mgh\x1b[0m",
	} {
		if got := output.String(); !strings.Contains(got, want) {
			t.Fatalf("colored prompt = %q, want substring %q", got, want)
		}
	}

	output.Reset()
	renderer = NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20})
	if err := renderer.Frame(LiveFrame{Prompt: PromptState{Prefix: "$ ", Text: "draft", Cursor: 5}}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\x1b[36;48;5;236m$\x1b[0m\x1b[38;5;252;48;5;236m \x1b[0m\x1b[32;48;5;236mdraft\x1b[0m"; !strings.Contains(got, want) {
		t.Fatalf("colored frame prompt = %q, want substring %q", got, want)
	}

	output.Reset()
	renderer = NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 2})
	if err := renderer.Prompt(PromptState{Prefix: "$ ", Text: "error:", Cursor: 6}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "\x1b[32;48;5;236mer\x1b[0m") || strings.Contains(got, "\x1b[31") {
		t.Fatalf("narrow prompt input did not remain green: %q", got)
	}
}

func TestLiveRendererStyledRowsKeepLeadingIndentWhenWrapped(t *testing.T) {
	var live, committed, narrow bytes.Buffer
	liveRenderer := NewLiveRenderer(&live, RendererConfig{TTY: true, Columns: 8})
	if err := liveRenderer.UpdateStyled([]StyledText{MutedText("  ○ abcdef")}); err != nil {
		t.Fatal(err)
	}
	if got, want := liveRenderer.rows, []string{"  ○ abcd", "  ef"}; !slices.Equal(got, want) {
		t.Fatalf("live rows = %#v, want %#v", got, want)
	}

	committedRenderer := NewLiveRenderer(&committed, RendererConfig{Columns: 8})
	if err := committedRenderer.CommitStyled(MutedText("  ○ abcdef")); err != nil {
		t.Fatal(err)
	}
	if got, want := committed.String(), "  ○ abcd\n  ef\n"; got != want {
		t.Fatalf("committed output = %q, want %q", got, want)
	}

	narrowRenderer := NewLiveRenderer(&narrow, RendererConfig{TTY: true, Columns: 4})
	if err := narrowRenderer.UpdateStyled([]StyledText{MutedText("   界x")}); err != nil {
		t.Fatal(err)
	}
	if got, want := narrowRenderer.rows, []string{"   ", "界x"}; !slices.Equal(got, want) {
		t.Fatalf("narrow rows = %#v, want %#v", got, want)
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
	for _, want := range []string{"- working response", "─", "$ next task  (○ pending)", "⠋ $ editable"} {
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
	if got := renderer.rows[2]; !strings.Contains(got, "─ mode: build ") || !strings.HasSuffix(got, " local/test ") {
		t.Fatalf("modeline labels are not padded: %q", got)
	}
}

func TestLiveRendererAlignsRowsAboveModeline(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20, MaxRows: 6})
	if err := renderer.Frame(LiveFrame{
		MessagePrefix: "● ", Message: "response", Context: []string{"context"}, Activity: []string{"✓ activity"},
		InputLeft: "mode: build", Prompt: PromptState{Prefix: "$ ", Text: "draft", Cursor: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 6 || renderer.rows[0] != "context" || renderer.rows[1] != "✓ activity" || renderer.rows[2] != "" ||
		renderer.rows[3] != "● response" || !strings.HasPrefix(renderer.rows[4], "─ mode: build ") || renderer.rows[5] != "$ draft" {
		t.Fatalf("live rows, modeline, and input are not aligned as expected: %#v", renderer.rows)
	}
	if got := output.String(); !strings.Contains(got, "\x1b[32;48;5;236m✓\x1b[0m\x1b[38;5;252;48;5;236m activity\x1b[0m") || !strings.Contains(got, "\x1b[38;5;195;48;5;236m● response\x1b[0m") {
		t.Fatalf("live rows lost semantic color: %q", got)
	}
	if got, modeline := output.String(), renderer.rows[4]; !strings.Contains(got, "\x1b[32;48;5;236m"+modeline+"\x1b[0m") {
		t.Fatalf("modeline was not green: %q", got)
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

func TestLiveRendererStylesLiveAssistantAfterEmptyLine(t *testing.T) {
	tests := []struct {
		name  string
		frame LiveFrame
	}{
		{name: "stream", frame: LiveFrame{Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "final answer"}}},
		{name: "message", frame: LiveFrame{MessagePrefix: "- ", Message: "final answer"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20, MaxRows: 6})
			if err := renderer.CommitUserMessage("$ ", "request"); err != nil {
				t.Fatal(err)
			}
			test.frame.Prompt = PromptState{Prefix: "$ "}
			if err := renderer.Frame(test.frame); err != nil {
				t.Fatal(err)
			}
			if len(renderer.rows) < 2 || renderer.rows[0] != "" || renderer.rows[1] != "- final answer" {
				t.Fatalf("final assistant boundary = %#v, want empty line then answer", renderer.rows)
			}
			if got := output.String(); !strings.Contains(got, "\x1b[38;5;195;48;5;236m- final answer\x1b[0m") || strings.Contains(got, "─") {
				t.Fatalf("final assistant did not use its role colors after an empty line: %q", got)
			}
		})
	}
}

func TestLiveRendererStylesCommittedAssistantAfterEmptyLine(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 20})
	if err := renderer.CommitStyled(MutedText("✓ summary")); err != nil {
		t.Fatal(err)
	}
	if err := renderer.CommitMessage("- ", "final answer", false); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[90m✓ summary\x1b[0m\n\n\x1b[38;5;195m- final answer\x1b[0m\n"
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

func TestLiveRendererPromotesCompleteStreamingLinesAndCommitsOnlySuffix(t *testing.T) {
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
	redraw := output.String()[before:]
	if strings.Contains(redraw, "- abcdefgh\n") || renderer.stream.started {
		t.Fatalf("width wrapping prematurely promoted Markdown source: output=%q state=%#v", redraw, renderer.stream)
	}
	if string(renderer.stream.pending) != "abcdefghijklmnop" {
		t.Fatalf("stream state = %#v", renderer.stream)
	}
	if len(renderer.rows) < 3 || renderer.rows[0] != "" || renderer.rows[1] != "- abcdefgh" || renderer.rows[2] != "  ijklmnop" {
		t.Fatalf("wrapped unfinished source is not live: %#v", renderer.rows)
	}
	if renderer.cursorRow != len(renderer.rows)-1 || !strings.Contains(strings.Join(renderer.rows, "\n"), "$ draft") {
		t.Fatalf("editor was not restored: rows=%#v cursor=%d", renderer.rows, renderer.cursorRow)
	}

	before = output.Len()
	frame.Stream.Text = "abcdefghijklmnop\nnext"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	promotion := output.String()[before:]
	if !strings.Contains(promotion, "- abcdefgh\n  ijklmnop\n") {
		t.Fatalf("complete source line was not promoted to scrollback: %q", promotion)
	}
	if !renderer.stream.started || string(renderer.stream.pending) != "next" || len(renderer.rows) == 0 || renderer.rows[0] != "  next" {
		t.Fatalf("promoted stream state = %#v; rows=%#v", renderer.stream, renderer.rows)
	}

	before = output.Len()
	if err := renderer.CommitStream(StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefghijklmnop\nnext!"}, true); err != nil {
		t.Fatal(err)
	}
	completion := output.String()[before:]
	if strings.Contains(completion, "abcdefgh") || !strings.Contains(completion, "  next!") {
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
	if got, want := renderer.rows[1], "- partial"; len(renderer.rows) < 2 || renderer.rows[0] != "" || got != want {
		t.Fatalf("highest live row = %q; want %q; rows=%#v", got, want, renderer.rows)
	}
	if context, activity := indexOf(renderer.rows, "question context"), indexOf(renderer.rows, "Tool: running"); context <= 0 || activity <= context {
		t.Fatalf("stream, context, and activity order is wrong: %#v", renderer.rows)
	}

	before := output.Len()
	frame.Stream.Text = "partial response that wraps\nnext"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	promotion := output.String()[before:]
	if !strings.Contains(promotion, "- partial response t\n  hat wraps\n") {
		t.Fatalf("top streaming row was not promoted at the live boundary: %q", promotion)
	}
	if len(renderer.rows) == 0 || renderer.rows[0] != "  next" {
		t.Fatalf("unfinished suffix is not the highest live row: %#v", renderer.rows)
	}
}

func TestLiveRendererCanCommitPreviouslyDisplayedStreamAfterTextDiverges(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 20, MaxRows: 6})
	frame := LiveFrame{Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "displayed suffix"}}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if err := renderer.CommitStream(StreamMessage{ID: "answer", Prefix: "- ", Text: "authoritative answer"}, false); RenderErrorClass(err) != "stream_text_changed" {
		t.Fatalf("divergent commit error = %v", err)
	}
	if err := renderer.CommitDisplayedStream("answer", false); err != nil {
		t.Fatal(err)
	}
	if renderer.stream.id != "" || !strings.Contains(output.String(), "displayed suffix") || strings.Contains(output.String(), "authoritative answer") {
		t.Fatalf("recovered stream state=%#v output=%q", renderer.stream, output.String())
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

func TestLiveRendererKeepsNewestActivityWithinTheLiveBudget(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxRows: DefaultLiveRows})
	activity := make([]StyledText, 12)
	for i := range activity {
		activity[i] = StyledText{Text: fmt.Sprintf("working %02d", i+1)}
	}
	if err := renderer.Frame(LiveFrame{StyledActivity: activity}); err != nil {
		t.Fatal(err)
	}
	// The live budget holds the ten newest rows; the empty prompt row that
	// follows belongs to the independently budgeted input region.
	if len(renderer.rows) != DefaultLiveRows+1 {
		t.Fatalf("frame rows = %d, want %d: %#v", len(renderer.rows), DefaultLiveRows+1, renderer.rows)
	}
	if renderer.rows[0] != "working 03" || renderer.rows[DefaultLiveRows-1] != "working 12" {
		t.Fatalf("oldest activity was not the clipped end: %#v", renderer.rows)
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

func TestLiveRendererSeparatesStreamAfterCompactCommitWithEmptyLine(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 10, MaxRows: 6})
	if err := renderer.CommitStyled(MutedText("✓ summary")); err != nil {
		t.Fatal(err)
	}
	frame := LiveFrame{Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "abc"}, Prompt: PromptState{Prefix: "$ "}}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) < 2 || renderer.rows[0] != "" || renderer.rows[1] != "- abc" {
		t.Fatalf("live stream is not preceded by an empty line: %#v", renderer.rows)
	}
	frame.Stream.Text = "abcdefghijklmnop"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if renderer.streamBlock {
		t.Fatal("width-only wrapping committed the stream block")
	}
	before := output.Len()
	frame.Stream.Text = "abcdefghijklmnop\nnext"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile("\\x1b(?:\\[\\?25[lh]|\\[2K|\\[[0-9]+[AB])").ReplaceAllString(output.String()[before:], "")
	plain = strings.ReplaceAll(plain, "\r", "")
	if strings.Contains(plain, "─") || !strings.Contains(plain, "- abcdefgh\n  ijklmnop\n") {
		t.Fatalf("stream promotion added a separator or lost content: output=%q", output.String())
	}
	if err := renderer.CommitStream(StreamMessage{ID: "answer", Prefix: "- ", Text: "abcdefghijklmnop\nnext!"}, false); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Commit("next tool"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(output.String(), "\nnext tool\n") {
		t.Fatalf("commit after stream was not block-separated: %q", output.String())
	}
}
