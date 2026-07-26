package tokenizer

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestCountAndTruncation(t *testing.T) {
	tokenizer, err := New()
	if err != nil {
		t.Fatal(err)
	}

	// o200k_base encodes this as two tokens (cl100k_base uses one).
	if got, err := tokenizer.Count("'RE"); err != nil || got != 2 {
		t.Fatalf("Count() = %d, %v; want 2, nil", got, err)
	}

	text := "one two three four five six"
	tests := []struct {
		name      string
		truncate  func(string, int) (string, int, int, error)
		budget    int
		want      string
		wantTotal int
		wantOmit  int
	}{
		{name: "middle", truncate: tokenizer.TruncateMiddle, budget: 3, want: "one\n…3 tokens truncated…\n five six", wantTotal: 6, wantOmit: 3},
		{name: "tail", truncate: tokenizer.TruncateTail, budget: 3, want: " four five six", wantTotal: 6, wantOmit: 3},
		{name: "middle within budget", truncate: tokenizer.TruncateMiddle, budget: 6, want: text, wantTotal: 6},
		{name: "tail under budget", truncate: tokenizer.TruncateTail, budget: 10, want: text, wantTotal: 6},
		{name: "middle zero", truncate: tokenizer.TruncateMiddle, budget: 0, wantTotal: 6, wantOmit: 6},
		{name: "tail zero", truncate: tokenizer.TruncateTail, budget: 0, wantTotal: 6, wantOmit: 6},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, total, omitted, err := test.truncate(text, test.budget)
			if err != nil || got != test.want || total != test.wantTotal || omitted != test.wantOmit {
				t.Fatalf("truncate() = %q, %d, %d, %v; want %q, %d, %d, nil", got, total, omitted, err, test.want, test.wantTotal, test.wantOmit)
			}
		})
	}
}

func TestTruncationReturnsValidUTF8AndExactOmissions(t *testing.T) {
	tokenizer, err := New()
	if err != nil {
		t.Fatal(err)
	}

	// The emoji spans multiple o200k_base tokens, so each truncation mode has
	// a budget that lands within a UTF-8 sequence.
	text := "prefix 🦜 suffix"
	for _, test := range []struct {
		name        string
		truncate    func(string, int) (string, int, int, error)
		budget      int
		want        string
		wantOmitted int
	}{
		{name: "middle", truncate: tokenizer.TruncateMiddle, budget: 4, want: "prefix\n…3 tokens truncated…\n suffix", wantOmitted: 3},
		{name: "tail", truncate: tokenizer.TruncateTail, budget: 3, want: " suffix", wantOmitted: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, total, omitted, err := test.truncate(text, test.budget)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncate() returned invalid UTF-8: %q", got)
			}
			if total != 5 || got != test.want || omitted != test.wantOmitted {
				t.Fatalf("truncate() = %q, total %d, omitted %d; want %q, 5, %d", got, total, omitted, test.want, test.wantOmitted)
			}
			if test.name == "middle" && !strings.Contains(got, "\n…"+strconv.Itoa(omitted)+" tokens truncated…\n") {
				t.Fatalf("truncate() = %q; missing exact omission marker", got)
			}
		})
	}
}

func TestNegativeBudget(t *testing.T) {
	tokenizer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, truncate := range []func(string, int) (string, int, int, error){tokenizer.TruncateMiddle, tokenizer.TruncateTail} {
		if _, _, _, err := truncate("text", -1); !errors.Is(err, ErrNegativeTokenBudget) {
			t.Fatalf("truncate() error = %v, want %v", err, ErrNegativeTokenBudget)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	tokenizer, err := New()
	if err != nil {
		t.Fatal(err)
	}

	const workers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, workers*2)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if count, err := tokenizer.Count(strings.Repeat("concurrent 🦜 text ", 20)); err != nil {
				errCh <- err
			} else if count == 0 {
				errCh <- errors.New("zero token count")
			}
			if text, _, _, err := tokenizer.TruncateMiddle(strings.Repeat("concurrent 🦜 text ", 20), 10); err != nil {
				errCh <- err
			} else if !utf8.ValidString(text) {
				errCh <- errors.New("invalid UTF-8")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
