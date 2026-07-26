// Package tokenizer provides exact token counting and token-budgeted text
// truncation using OpenAI's o200k_base encoding.
package tokenizer

import (
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	tiktoken "github.com/tiktoken-go/tokenizer"
)

var ErrNegativeTokenBudget = errors.New("token budget must not be negative")

// Tokenizer counts and truncates text using o200k_base. A Tokenizer is safe
// for concurrent use.
type Tokenizer struct {
	codec tiktoken.Codec
	mu    sync.Mutex
}

// New creates an o200k_base tokenizer.
func New() (*Tokenizer, error) {
	codec, err := tiktoken.Get(tiktoken.O200kBase)
	if err != nil {
		return nil, err
	}
	return &Tokenizer{codec: codec}, nil
}

// Count returns the exact number of o200k_base tokens in text.
func (t *Tokenizer) Count(text string) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.codec.Count(text)
}

// TruncateMiddle retains tokens from both ends of text, using half the budget
// for the start and the remainder for the end. It separates them with an
// omission marker, whose tokens are not charged to the source-token budget.
// totalTokens counts the source; omittedTokens includes every source token not
// represented in textOut.
func (t *Tokenizer) TruncateMiddle(text string, maxTokens int) (textOut string, totalTokens, omittedTokens int, err error) {
	return t.truncate(text, maxTokens, true)
}

// TruncateTail retains the final maxTokens tokens of text. totalTokens counts
// the source; omittedTokens includes every source token not represented in
// textOut.
func (t *Tokenizer) TruncateTail(text string, maxTokens int) (textOut string, totalTokens, omittedTokens int, err error) {
	return t.truncate(text, maxTokens, false)
}

func (t *Tokenizer) truncate(text string, maxTokens int, middle bool) (string, int, int, error) {
	if maxTokens < 0 {
		return "", 0, 0, ErrNegativeTokenBudget
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	ids, _, err := t.codec.Encode(text)
	if err != nil {
		return "", 0, 0, err
	}
	totalTokens := len(ids)
	if totalTokens <= maxTokens {
		return text, totalTokens, 0, nil
	}
	if maxTokens == 0 {
		return "", totalTokens, totalTokens, nil
	}

	if !middle {
		out, retained, err := t.validTail(ids[totalTokens-maxTokens:])
		return out, totalTokens, totalTokens - retained, err
	}

	head, headTokens, err := t.validHead(ids[:maxTokens/2])
	if err != nil {
		return "", 0, 0, err
	}
	tail, tailTokens, err := t.validTail(ids[totalTokens-(maxTokens-maxTokens/2):])
	if err != nil {
		return "", 0, 0, err
	}
	omittedTokens := totalTokens - headTokens - tailTokens
	return head + fmt.Sprintf("\n…%d tokens truncated…\n", omittedTokens) + tail, totalTokens, omittedTokens, nil
}

func (t *Tokenizer) validHead(ids []uint) (string, int, error) {
	for len(ids) > 0 {
		text, err := t.codec.Decode(ids)
		if err != nil {
			return "", 0, err
		}
		if utf8.ValidString(text) {
			return text, len(ids), nil
		}
		ids = ids[:len(ids)-1]
	}
	return "", 0, nil
}

func (t *Tokenizer) validTail(ids []uint) (string, int, error) {
	for len(ids) > 0 {
		text, err := t.codec.Decode(ids)
		if err != nil {
			return "", 0, err
		}
		if utf8.ValidString(text) {
			return text, len(ids), nil
		}
		ids = ids[1:]
	}
	return "", 0, nil
}
