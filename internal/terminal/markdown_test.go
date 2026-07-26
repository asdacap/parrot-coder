package terminal

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var sgrPattern = regexp.MustCompile("\\x1b\\[[0-9;]*m")

func TestRenderAssistantMarkdownFormatsBlockAndInlineSyntax(t *testing.T) {
	rendered := renderAssistantMarkdown("- ", strings.Join([]string{
		"# Heading",
		"> quoted **text**",
		"- [x] done",
		"1. *item*",
		"Text with ~~old~~, `code`, and [docs](https://example.com).",
		"---",
	}, "\n"), 80, true)

	want := []string{
		"- Heading",
		"  │ quoted text",
		"  • ☑ done",
		"  1. item",
		"  Text with old, code, and docs (https://example.com).",
		"  " + strings.Repeat("─", 78),
	}
	if got := rendered.rows; !equalMarkdownRows(got, want) {
		t.Fatalf("Markdown rows = %#v; want %#v", got, want)
	}
	for _, marker := range []string{"# ", "**", "~~", "`code`", "[docs]("} {
		if strings.Contains(strings.Join(rendered.rows, "\n"), marker) {
			t.Fatalf("rendered Markdown retained marker %q: %#v", marker, rendered.rows)
		}
	}
	if !hasANSIStyle(rendered, func(style ansiStyle) bool { return style.bold && style.color == "36" }) {
		t.Fatal("heading style was not retained as renderer metadata")
	}
	if !hasANSIStyle(rendered, func(style ansiStyle) bool { return style.strike }) {
		t.Fatal("strikethrough style was not retained as renderer metadata")
	}
	if !hasANSIStyle(rendered, func(style ansiStyle) bool { return style.underline }) {
		t.Fatal("link style was not retained as renderer metadata")
	}
}

func TestAssistantMarkdownWrapsProseAtWords(t *testing.T) {
	for _, test := range []struct {
		name    string
		source  string
		columns int
		want    []string
	}{
		{
			name:    "ordinary prose",
			source:  "alpha beta gamma",
			columns: 12,
			want:    []string{"- alpha beta", "  gamma"},
		},
		{
			name:    "overlong token",
			source:  "abcdefghijk tail",
			columns: 8,
			want:    []string{"- abcdef", "  ghijk", "  tail"},
		},
		{
			name:    "non-breaking space remains attached",
			source:  "one\u00a0two tail",
			columns: 10,
			want:    []string{"- one\u00a0two", "  tail"},
		},
		{
			name:    "fenced code retains hard wrapping",
			source:  "```text\nalpha beta\n```",
			columns: 8,
			want:    []string{"- alpha ", "  beta"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := renderAssistantMarkdown("- ", test.source, test.columns, true)
			if !equalMarkdownRows(rendered.rows, test.want) {
				t.Fatalf("wrapped rows = %#v; want %#v", rendered.rows, test.want)
			}
			for _, row := range rendered.rows {
				if displayWidth(row) > test.columns {
					t.Fatalf("wrapped row exceeded terminal width: %q", row)
				}
			}
		})
	}

	styled := renderAssistantMarkdown("- ", "one **styled 東京** tail", 12, true)
	if want := []string{"- one styled", "  東京 tail"}; !equalMarkdownRows(styled.rows, want) {
		t.Fatalf("styled Unicode rows = %#v; want %#v", styled.rows, want)
	}
	for row, want := range []string{"styled", "東京"} {
		found := false
		for _, span := range styled.spans[row] {
			if span.style.bold && styled.rows[row][span.start:span.end] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("bold style for %q was not retained across wrap: rows=%#v spans=%#v", want, styled.rows, styled.spans)
		}
	}
}

func TestRenderAssistantMarkdownFormatsTables(t *testing.T) {
	rendered := renderAssistantMarkdown("- ", strings.Join([]string{
		"| Name | Count | State |",
		"| :--- | ---: | :---: |",
		"| **A** | 7 | ok |",
		"| 東京 | 12 | `a|b` |",
		"| escaped \\| pipe | 3 | ready |",
	}, "\n"), 50, true)

	want := []string{
		"- ┌────────────────┬───────┬───────┐",
		"  │ Name           │ Count │ State │",
		"  ├────────────────┼───────┼───────┤",
		"  │ A              │     7 │  ok   │",
		"  │ 東京           │    12 │  a|b  │",
		"  │ escaped | pipe │     3 │ ready │",
		"  └────────────────┴───────┴───────┘",
	}
	if !equalMarkdownRows(rendered.rows, want) {
		t.Fatalf("table rows = %#v; want %#v", rendered.rows, want)
	}
	if !hasANSIStyle(rendered, func(style ansiStyle) bool { return style.bold && style.color == "36" }) ||
		!hasANSIStyle(rendered, func(style ansiStyle) bool { return style.bold }) {
		t.Fatal("table header or inline Markdown style was not retained")
	}
	for _, row := range rendered.rows {
		if displayWidth(row) > 50 {
			t.Fatalf("table row exceeded terminal width: %q", row)
		}
	}

	for _, test := range []struct {
		name    string
		source  string
		columns int
		want    string
	}{
		{name: "narrow", source: "A | B | C\n--- | --- | ---\none | two | three", columns: 12, want: "- A: one\n  B: two\n  C: three"},
		{name: "wide rune cannot fit grid cell", source: "A | B\n--- | ---\n字 | x", columns: 11, want: "- A: 字\n  B: x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			narrow := renderAssistantMarkdown("- ", test.source, test.columns, false)
			if got := strings.Join(narrow.rows, "\n"); got != test.want {
				t.Fatalf("table fallback = %q; want %q", got, test.want)
			}
			for _, row := range narrow.rows {
				if displayWidth(row) > test.columns {
					t.Fatalf("fallback row exceeded terminal width: %q", row)
				}
			}
		})
	}
	if _, ok := parseTableDelimiter("::--- | ---", 2); ok {
		t.Fatal("delimiter with repeated alignment colons was accepted")
	}

	ordinary := renderMarkdown("- ", "left | right", " suffix", 50, false)
	if got := strings.Join(ordinary.rows, "\n"); got != "- left | right suffix" {
		t.Fatalf("unconfirmed table row = %q", got)
	}

	single := renderMarkdown("- ", "| Name |\n| --- |\n| A |\nAfter", " suffix", 30, false)
	singleText := strings.Join(single.rows, "\n")
	if !strings.Contains(singleText, "│ Name │") || !strings.HasSuffix(singleText, "After suffix") {
		t.Fatalf("single-column table or boundary suffix = %q", singleText)
	}
	fenceAfterCandidate := renderMarkdown("- ", "A | B\n```\ncode", " suffix", 30, false)
	if got := strings.Count(strings.Join(fenceAfterCandidate.rows, "\n"), " suffix"); got != 1 {
		t.Fatalf("table candidate before fence rendered suffix %d times: %#v", got, fenceAfterCandidate.rows)
	}
}

func TestStreamBuffersMarkdownTableUntilItsBoundary(t *testing.T) {
	renderer := NewLiveRenderer(&bytes.Buffer{}, RendererConfig{TTY: true, Columns: 50, MaxRows: 20})
	message := StreamMessage{ID: "table", Prefix: "- ", Text: "Name | Value\n--- | ---:\na | 1\n"}
	promoted, preview, err := renderer.advanceStream(message, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted.rows) != 0 || !renderer.stream.markdown.table.pending() {
		t.Fatalf("table was promoted before its boundary: promoted=%#v state=%#v", promoted.rows, renderer.stream.markdown.table)
	}
	if got := strings.Join(preview.rows, "\n"); !strings.Contains(got, "│ Name │ Value │") || !strings.Contains(got, "│ a    │     1 │") {
		t.Fatalf("table preview = %q", got)
	}

	message.Text += "longer value | 200\n"
	promoted, preview, err = renderer.advanceStream(message, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted.rows) != 0 || !strings.Contains(strings.Join(preview.rows, "\n"), "│ Name         │ Value │") {
		t.Fatalf("later row did not realign buffered table: promoted=%#v preview=%#v", promoted.rows, preview.rows)
	}

	message.Text += "After\n"
	promoted, _, err = renderer.advanceStream(message, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(promoted.rows, "\n")
	if renderer.stream.markdown.table.pending() || !strings.Contains(joined, "│ longer value │   200 │") || !strings.HasSuffix(joined, "\n  After") {
		t.Fatalf("table boundary promotion = %q; state=%#v", joined, renderer.stream.markdown.table)
	}

	finishing := NewLiveRenderer(&bytes.Buffer{}, RendererConfig{TTY: true, Columns: 50})
	message = StreamMessage{ID: "finish", Prefix: "- ", Text: "A | B\n--- | ---\nx | y"}
	promoted, _, err = finishing.advanceStream(message, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(promoted.rows, "\n"); !strings.Contains(got, "│ x │ y │") || finishing.stream.markdown.table.pending() {
		t.Fatalf("completed table was not flushed: %q", got)
	}

	bounded := markdownState{tableRowLimit: maxMarkdownTableRows}
	renderMarkdownLine("- ", "A | B", 50, &bounded, false)
	renderMarkdownLine("- ", "--- | ---", 50, &bounded, false)
	var overflow richRows
	for range maxMarkdownTableRows {
		overflow.append(renderMarkdownLine("- ", "x | y", 50, &bounded, false))
	}
	if !bounded.table.plain || len(bounded.table.rows) != 0 || !strings.Contains(strings.Join(overflow.rows, "\n"), "│ x │ y │") {
		t.Fatalf("large table did not switch to bounded plain streaming: state=%#v rows=%d", bounded.table, len(overflow.rows))
	}
	plain := renderMarkdownLine("  ", "next | row", 50, &bounded, false)
	if got := strings.Join(plain.rows, "\n"); got != "  next | row" {
		t.Fatalf("large table plain continuation = %q", got)
	}
}

func TestStyledMarkdownRendersInLiveAndCommittedActivity(t *testing.T) {
	activity := StyledText{Text: "# Checking\n\n- **tests**", Markdown: true, Prefix: "✓ ", Suffix: " · 2 tokens"}
	for _, test := range []struct {
		name   string
		render func(*LiveRenderer) error
		styles []string
	}{
		{name: "live", render: func(renderer *LiveRenderer) error {
			return renderer.Frame(LiveFrame{StyledActivity: []StyledText{activity}})
		}, styles: []string{"\x1b[1;36;48;5;236mChecking\x1b[0m", "\x1b[1;38;5;252;48;5;236mtests\x1b[0m"}},
		{name: "committed", render: func(renderer *LiveRenderer) error {
			return renderer.CommitStyled(activity)
		}, styles: []string{"\x1b[1;36mChecking\x1b[0m", "\x1b[1mtests\x1b[0m"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 80})
			if err := test.render(renderer); err != nil {
				t.Fatal(err)
			}
			plain := sgrPattern.ReplaceAllString(output.String(), "")
			if !strings.Contains(plain, "✓ Checking") || !strings.Contains(plain, "  • tests · 2 tokens") || strings.Contains(plain, "# Checking") || strings.Contains(plain, "**") {
				t.Fatalf("styled Markdown output = %q", plain)
			}
			for _, style := range test.styles {
				if !strings.Contains(output.String(), style) {
					t.Fatalf("styled Markdown metadata was not rendered: %q", output.String())
				}
			}
		})
	}
}

func TestLiveStyledMarkdownPreviewIsBounded(t *testing.T) {
	source := "```go\n" + strings.Repeat("old_line()\n", maxLiveMarkdownLines*4) + "```\n# Latest"
	preview, truncated := liveMarkdownPreview(source)
	if !truncated || utf8.RuneCountInString(preview) > maxLiveMarkdownRunes || strings.Count(preview, "\n") >= maxLiveMarkdownLines {
		t.Fatalf("live preview was not bounded: truncated=%v, runes=%d, lines=%d", truncated, utf8.RuneCountInString(preview), strings.Count(preview, "\n")+1)
	}

	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 80})
	if err := renderer.UpdateStyled([]StyledText{{Text: source, Markdown: true, Prefix: "✓ "}}); err != nil {
		t.Fatal(err)
	}
	plain := sgrPattern.ReplaceAllString(output.String(), "")
	if !strings.Contains(plain, "# Latest") || len(renderer.rows) > renderer.maxRows {
		t.Fatalf("bounded Markdown preview = %q", plain)
	}
}

func TestLiveRendererHighlightsFencedCodeAndHidesDelimiters(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: true, Columns: 80})
	source := "Before\n```go\npackage main\nfunc main() { println(\"hello\") }\n```\nAfter"
	if err := renderer.CommitMessage("- ", source, false); err != nil {
		t.Fatal(err)
	}

	plain := sgrPattern.ReplaceAllString(output.String(), "")
	want := "\n- Before\n  package main\n  func main() { println(\"hello\") }\n  After\n"
	if plain != want {
		t.Fatalf("highlighted code plain text = %q; want %q", plain, want)
	}
	if strings.Contains(output.String(), "```") {
		t.Fatalf("fence delimiter was rendered: %q", output.String())
	}
	if !strings.Contains(output.String(), "\x1b[1;35mpackage\x1b[0m") ||
		!strings.Contains(output.String(), "\x1b[32m\"hello\"\x1b[0m") {
		t.Fatalf("Go syntax was not highlighted: %q", output.String())
	}
}

func TestAssistantMarkdownKeepsRoleColorsOnPrefix(t *testing.T) {
	rendered := renderAssistantMarkdown("- ", "**answer**", 80, true)
	renderer := NewLiveRenderer(&bytes.Buffer{}, RendererConfig{TTY: true, Color: true, Columns: 80})
	got := renderer.decorateRich(rendered.rows[0], textStyleAssistantMessage, rendered.spans[0])
	want := "\x1b[38;5;195m- \x1b[0m\x1b[1;38;5;195manswer\x1b[0m"
	if got != want {
		t.Fatalf("styled assistant Markdown = %q; want %q", got, want)
	}
}

func TestFencedHighlightingPreservesMultilineLexerState(t *testing.T) {
	rendered := renderAssistantMarkdown("", "```python\nvalue = \"\"\"hello\nworld\"\"\"\n```", 80, true)
	if got, want := strings.Join(rendered.rows, "\n"), "value = \"\"\"hello\nworld\"\"\""; got != want {
		t.Fatalf("multiline code rows = %q; want %q", got, want)
	}

	renderer := NewLiveRenderer(&bytes.Buffer{}, RendererConfig{TTY: true, Color: true, Columns: 80})
	first := renderer.decorateRich(rendered.rows[0], TextStyleDefault, rendered.spans[0])
	second := renderer.decorateRich(rendered.rows[1], TextStyleDefault, rendered.spans[1])
	if !strings.Contains(first, "\x1b[32m\"\"\"hello\x1b[0m") ||
		!strings.Contains(second, "\x1b[32mworld\"\"\"\x1b[0m") {
		t.Fatalf("multiline Python string lost lexer state: first=%q second=%q", first, second)
	}
}

func TestStreamBuffersFencedCodeUntilClosingFence(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: false, Columns: 40, MaxRows: 6})
	frame := LiveFrame{
		Stream: &StreamMessage{ID: "answer", Prefix: "- ", Text: "```python\nvalue = \"\"\"hello\n"},
		Prompt: PromptState{Prefix: "$ "},
	}
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if renderer.stream.started || !renderer.stream.markdown.inFence || len(renderer.stream.markdown.code) != 1 {
		t.Fatalf("open fence state = %#v", renderer.stream)
	}
	if len(renderer.rows) < 3 || renderer.rows[0] != "" || renderer.rows[1] != "- value = \"\"\"hello" {
		t.Fatalf("open fence preview = %#v", renderer.rows)
	}

	frame.Stream.Text += "world\"\"\"\n"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	if renderer.stream.started || len(renderer.stream.markdown.code) != 2 {
		t.Fatalf("fenced source was promoted before close: %#v", renderer.stream)
	}

	before := output.Len()
	frame.Stream.Text += "```\nAfter"
	if err := renderer.Frame(frame); err != nil {
		t.Fatal(err)
	}
	promotion := output.String()[before:]
	if !strings.Contains(promotion, "- value = \"\"\"hello\n  world\"\"\"\n") || strings.Contains(promotion, "```") {
		t.Fatalf("closed fence promotion = %q", promotion)
	}
	if !renderer.stream.started || renderer.stream.markdown.inFence || string(renderer.stream.pending) != "After" || renderer.rows[0] != "  After" {
		t.Fatalf("state after closing fence = %#v; rows=%#v", renderer.stream, renderer.rows)
	}

	before = output.Len()
	if err := renderer.CommitStream(StreamMessage{ID: "answer", Prefix: "- ", Text: frame.Stream.Text + "!"}, false); err != nil {
		t.Fatal(err)
	}
	completion := output.String()[before:]
	if strings.Contains(completion, "value") || !strings.Contains(completion, "  After!") {
		t.Fatalf("fenced stream completion duplicated or lost source: %q", completion)
	}
}

func TestCommitStreamFlushesUnclosedFence(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Color: false, Columns: 80})
	message := StreamMessage{ID: "answer", Prefix: "- ", Text: "```go\npackage main"}
	if err := renderer.Frame(LiveFrame{Stream: &message, Prompt: PromptState{Prefix: "$ "}}); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	if err := renderer.CommitStream(message, false); err != nil {
		t.Fatal(err)
	}
	completion := output.String()[before:]
	if !strings.Contains(completion, "- package main\n") || strings.Contains(completion, "```") {
		t.Fatalf("unclosed fence completion = %q", completion)
	}
}

func TestCodeHighlightingAliasesPlainFallbacksAndLimits(t *testing.T) {
	for _, language := range []string{"c-sharp", "c#", "golang", "python3", "py3", "shell", "sh", "zsh"} {
		if lexerForLanguage(language) == nil {
			t.Errorf("language alias %q did not resolve", language)
		}
	}
	for _, language := range []string{"", "text", "plaintext", "plain", "not-a-language"} {
		if lexerForLanguage(language) != nil {
			t.Errorf("plain or unknown language %q unexpectedly resolved", language)
		}
	}

	unknown := renderAssistantMarkdown("", "```not-a-language\nlet value = 1\n```", 80, true)
	noColor := renderAssistantMarkdown("", "```go\npackage main\n```", 80, false)
	if hasANSIStyle(unknown, func(ansiStyle) bool { return true }) || hasANSIStyle(noColor, func(ansiStyle) bool { return true }) {
		t.Fatal("unknown-language or no-color code received highlight spans")
	}

	lexer := lexerForLanguage("go")
	oversized := renderCodeBlock("", markdownState{
		lexer: lexer, code: []string{strings.Repeat("x", maxHighlightBytes+1)}, codeBytes: maxHighlightBytes + 1,
	}, maxHighlightBytes+2, true)
	tooManyLines := renderCodeBlock("", markdownState{
		lexer: lexer, code: make([]string, maxHighlightLines+1), codeBytes: maxHighlightLines,
	}, 80, true)
	if hasANSIStyle(oversized, func(ansiStyle) bool { return true }) || hasANSIStyle(tooManyLines, func(ansiStyle) bool { return true }) {
		t.Fatal("code beyond highlighting limits received highlight spans")
	}
}

func TestAssistantMarkdownNoColorNonTTYAndSanitization(t *testing.T) {
	var plainOutput bytes.Buffer
	plainRenderer := NewLiveRenderer(&plainOutput, RendererConfig{TTY: false, Color: true, Columns: 80})
	if err := plainRenderer.CommitMessage("- ", "**bold**\n```go\npackage main\n```", false); err != nil {
		t.Fatal(err)
	}
	if got, want := plainOutput.String(), "\n- bold\n  package main\n"; got != want {
		t.Fatalf("non-TTY Markdown = %q; want %q", got, want)
	}
	if strings.ContainsRune(plainOutput.String(), '\x1b') {
		t.Fatalf("non-TTY Markdown emitted ANSI: %q", plainOutput.String())
	}

	var colorOutput bytes.Buffer
	colorRenderer := NewLiveRenderer(&colorOutput, RendererConfig{TTY: true, Color: true, Columns: 80})
	source := "# unsafe\x1b[2J\n```go\nprintln(\"\x1b]0;owned\a\")\n```"
	if err := colorRenderer.CommitMessage("- ", source, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(colorOutput.String(), "\x1b[2J") || strings.Contains(colorOutput.String(), "\x1b]") {
		t.Fatalf("untrusted terminal control survived Markdown rendering: %q", colorOutput.String())
	}
	withoutRendererSGR := sgrPattern.ReplaceAllString(colorOutput.String(), "")
	if strings.ContainsRune(withoutRendererSGR, '\x1b') {
		t.Fatalf("Markdown emitted a non-renderer escape: %q", colorOutput.String())
	}
}

func TestAssistantMarkdownPreservesOrdinaryPunctuationAndMalformedSyntax(t *testing.T) {
	source := strings.Join([]string{
		`C:\Users\name and snake_case and file*name*value`,
		`Escaped \*literal\* and unfinished *emphasis\*`,
		strings.Repeat("[", 128*1024),
	}, "\n")
	rendered := renderAssistantMarkdown("- ", source, 200_000, false)
	got := strings.Join(rendered.rows, "\n")
	for _, want := range []string{
		`- C:\Users\name and snake_case and file*name*value`,
		`  Escaped *literal* and unfinished *emphasis\*`,
		"  " + strings.Repeat("[", 128*1024),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered Markdown lost or reinterpreted %q", want)
		}
	}
}

func TestStreamingMarkdownPreviewBoundsUnfinishedTextAndOpenFence(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "unfinished line", text: strings.Repeat("x", maxLiveMarkdownRunes*8)},
		{name: "open fence", text: "```go\n" + strings.Repeat("package_name += value\n", maxLiveMarkdownLines*4)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 40, MaxRows: 3})
			message := StreamMessage{ID: "answer", Prefix: "- ", Text: test.text}
			if err := renderer.Frame(LiveFrame{Stream: &message, Prompt: PromptState{Prefix: "$ "}}); err != nil {
				t.Fatal(err)
			}
			if len(renderer.rows) > 4 { // three stream rows plus the prompt
				t.Fatalf("live preview exceeded its row budget: %d rows", len(renderer.rows))
			}
			for _, row := range renderer.rows {
				if displayWidth(row) > 40 {
					t.Fatalf("live preview row exceeded terminal width: %q", row)
				}
			}
		})
	}
}

func hasANSIStyle(rows richRows, match func(ansiStyle) bool) bool {
	for _, spans := range rows.spans {
		for _, span := range spans {
			if match(span.style) {
				return true
			}
		}
	}
	return false
}

func equalMarkdownRows(left, right []string) bool {
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
