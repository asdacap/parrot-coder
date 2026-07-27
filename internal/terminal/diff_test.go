package terminal

import (
	"strings"
	"testing"
)

const testTraditionalDiff = `--- old/a.txt
+++ new/a.txt
@@ -1,2 +1,2 @@
 keep
-old
+new
--- old/b.txt
+++ new/b.txt
@@ -10,2 +10,3 @@
 same
-left
+right
+extra
`

func TestFormatDiffSideBySide(t *testing.T) {
	formatted := formatDiff(testTraditionalDiff, 80, false)
	if len(formatted.rows) != 9 {
		t.Fatalf("got %d rows, want 9: %#v", len(formatted.rows), formatted.rows)
	}
	checks := []struct {
		row      int
		contains []string
	}{
		{0, []string{"new/a.txt"}},
		{1, []string{"@@ -1,2 +1,2 @@"}},
		{2, []string{"1  keep", "│", "1  keep"}},
		{3, []string{"2 -old", "│", "2 +new"}},
		{4, []string{"new/b.txt"}},
		{5, []string{"@@ -10,2 +10,3 @@"}},
		{6, []string{"10  same", "│", "10  same"}},
		{7, []string{"11 -left", "11 +right"}},
	}
	for _, check := range checks {
		for _, value := range check.contains {
			if !strings.Contains(formatted.rows[check.row], value) {
				t.Errorf("row %d does not contain %q: %q", check.row, value, formatted.rows[check.row])
			}
		}
		if displayWidth(formatted.rows[check.row]) > 80 {
			t.Errorf("row %d exceeds width: %d", check.row, displayWidth(formatted.rows[check.row]))
		}
	}
	if len(formatted.spans[3]) != 2 || formatted.spans[3][0].style.color != "31" || formatted.spans[3][1].style.color != "32" {
		t.Fatalf("replacement spans = %#v", formatted.spans[3])
	}
}

func TestFormatDiffInline(t *testing.T) {
	formatted := formatDiff(testTraditionalDiff, 80, true)
	joined := strings.Join(formatted.rows, "\n")
	for _, want := range []string{"new/a.txt", "@@ -1,2 +1,2 @@", " 1  keep", " 2 -old", " 2 +new", "11 -left", "11 +right"} {
		if !strings.Contains(joined, want) {
			t.Errorf("inline diff omitted %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "│") {
		t.Fatalf("inline diff contains side-by-side gutter: %q", joined)
	}
	if len(formatted.spans[3]) != 1 || formatted.spans[3][0].start != 0 || formatted.spans[3][0].end != len(formatted.rows[3]) || formatted.spans[3][0].style.color != "31" || len(formatted.spans[4]) != 1 || formatted.spans[4][0].start != 0 || formatted.spans[4][0].end != len(formatted.rows[4]) || formatted.spans[4][0].style.color != "32" {
		t.Fatalf("replacement spans = %#v / %#v", formatted.spans[3], formatted.spans[4])
	}
	for _, row := range formatted.rows {
		if displayWidth(row) > 80 {
			t.Fatalf("row exceeds width: %q", row)
		}
	}
	for _, row := range formatDiff(testTraditionalDiff, 2, true).rows {
		if displayWidth(row) > 2 {
			t.Fatalf("narrow inline row exceeds width: %q", row)
		}
	}
}

func TestFormatDiffPreservesMultipleHunkBoundaries(t *testing.T) {
	raw := "--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old one\n+new one\n@@ -20 +20 @@\n-old twenty\n+new twenty\n"
	joined := strings.Join(formatDiff(raw, 80, false).rows, "\n")
	for _, want := range []string{"b/file.go", "@@ -1,1 +1,1 @@", "@@ -20,1 +20,1 @@", "1 -old one", "20 -old twenty"} {
		if !strings.Contains(joined, want) {
			t.Errorf("formatted diff omitted %q: %q", want, joined)
		}
	}
}

func TestFormatDiffBoundsWidthAndFallback(t *testing.T) {
	var many strings.Builder
	many.WriteString("--- old/a.txt\n+++ new/a.txt\n@@ -1,102 +1,102 @@\n")
	for index := 0; index < 101; index++ {
		many.WriteString(" line\t界界界界界界\n")
	}
	many.WriteString("-old\n+new\n")

	tests := []struct {
		name       string
		raw        string
		columns    int
		wantRows   int
		wantText   string
		wantGutter bool
	}{
		{name: "bounded side by side", raw: many.String(), columns: 41, wantRows: 101, wantText: "4 diff rows omitted", wantGutter: true},
		{name: "narrow unified", raw: many.String(), columns: 20, wantRows: 101, wantText: "6 omitted", wantGutter: false},
		{name: "malformed unified", raw: "--- a\n+++ b\n@@ broken\n-old\n+new", columns: 60, wantRows: 5, wantText: "@@ broken", wantGutter: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			formatted := formatDiff(test.raw, test.columns, false)
			if len(formatted.rows) != test.wantRows {
				t.Fatalf("got %d rows, want %d: %#v", len(formatted.rows), test.wantRows, formatted.rows)
			}
			joined := strings.Join(formatted.rows, "\n")
			if !strings.Contains(joined, test.wantText) || strings.Contains(joined, "\t") {
				t.Fatalf("unexpected output: %q", joined)
			}
			if strings.Contains(joined, "│") != test.wantGutter {
				t.Fatalf("gutter presence = %v in %q", strings.Contains(joined, "│"), joined)
			}
			for _, row := range formatted.rows {
				if displayWidth(row) > test.columns {
					t.Fatalf("row width %d > %d: %q", displayWidth(row), test.columns, row)
				}
			}
		})
	}
}

type atomicDiffWriter struct {
	writes int
	text   strings.Builder
}

func (w *atomicDiffWriter) Write(value []byte) (int, error) {
	w.writes++
	return w.text.Write(value)
}

func TestCommitDiffBlockIsAtomicAndRendererStyled(t *testing.T) {
	writer := &atomicDiffWriter{}
	renderer := NewLiveRenderer(writer, RendererConfig{TTY: true, Color: true, Columns: 80, InlineDiff: true})
	if err := renderer.CommitDiffBlock(MutedText("changed files"), testTraditionalDiff); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}
	output := writer.text.String()
	for _, value := range []string{"changed files", "\x1b[31m 2 -", "\x1b[32m 2 +", "\x1b[31m", "\x1b[32m"} {
		if !strings.Contains(output, value) {
			t.Errorf("output does not contain %q: %q", value, output)
		}
	}
	if strings.Contains(output, " │ ") {
		t.Fatalf("inline diff contains side-by-side gutter: %q", output)
	}
	if strings.Contains(testTraditionalDiff, "\x1b[") {
		t.Fatal("test diff unexpectedly owns ANSI styling")
	}
}
