package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode"
	"unicode/utf8"
)

// KeyKind identifies a decoded terminal input event.
type KeyKind uint8

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyDelete
	KeyBackspace
	KeyEnter
	KeyNewline
	KeyTab
	KeyEscape
	KeyInterrupt
	KeyEOF
	KeyPaste
)

// Key is one decoded input event. Text is populated for KeyPaste.
type Key struct {
	Kind KeyKind
	Rune rune
	Text string
}

// ContextReader lets ReadKey unblock promptly when its context is canceled.
// Ordinary io.Reader values remain supported, but cannot always be interrupted.
type ContextReader interface {
	ReadContext(context.Context, []byte) (int, error)
}

// KeyDecoder decodes UTF-8 text and common terminal key sequences.
type KeyDecoder struct {
	reader   io.Reader
	pending  []byte
	readErr  error
	maxPaste int
	timedTTY bool
}

// NewKeyDecoder returns a decoder with bounded bracketed-paste input.
func NewKeyDecoder(reader io.Reader) *KeyDecoder {
	file, _ := reader.(*os.File)
	return &KeyDecoder{reader: reader, maxPaste: 64 << 10, timedTTY: IsTTY(file)}
}

// SetMaxPasteBytes changes the bracketed-paste byte limit. Non-positive
// values restore the default.
func (d *KeyDecoder) SetMaxPasteBytes(limit int) {
	if limit <= 0 {
		limit = 64 << 10
	}
	d.maxPaste = limit
}

// ReadKey reads one key. It never starts a background goroutine.
func (d *KeyDecoder) ReadKey(ctx context.Context) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	b, err := d.readByte(ctx)
	if err != nil {
		return Key{}, err
	}
	switch b {
	case 0x03:
		return Key{Kind: KeyInterrupt}, nil
	case 0x04:
		return Key{Kind: KeyEOF}, nil
	case '\t':
		return Key{Kind: KeyTab}, nil
	case '\n':
		return Key{Kind: KeyNewline}, nil
	case '\r':
		return Key{Kind: KeyEnter}, nil
	case 0x7f, 0x08:
		return Key{Kind: KeyBackspace}, nil
	case 0x1b:
		return d.readEscape(ctx)
	}
	if b < utf8.RuneSelf {
		if b < 0x20 || b == 0x7f {
			return Key{}, fmt.Errorf("terminal: rejected control byte 0x%02x", b)
		}
		return Key{Kind: KeyRune, Rune: rune(b)}, nil
	}

	r, err := d.readRune(ctx, b)
	if err != nil {
		return Key{}, err
	}
	if unicode.IsControl(r) {
		return Key{}, fmt.Errorf("terminal: rejected control character %U", r)
	}
	return Key{Kind: KeyRune, Rune: r}, nil
}

func (d *KeyDecoder) readEscape(ctx context.Context) (Key, error) {
	b, ok, err := d.readByteOnce(ctx)
	if !ok && err == nil {
		return Key{Kind: KeyEscape}, nil
	}
	if errors.Is(err, io.EOF) {
		return Key{Kind: KeyEscape}, nil
	}
	if err != nil {
		return Key{}, err
	}
	if b != '[' {
		d.unreadByte(b)
		return Key{Kind: KeyEscape}, nil
	}
	b, err = d.readByte(ctx)
	if err != nil {
		return Key{}, err
	}
	switch b {
	case 'A':
		return Key{Kind: KeyUp}, nil
	case 'B':
		return Key{Kind: KeyDown}, nil
	case 'C':
		return Key{Kind: KeyRight}, nil
	case 'D':
		return Key{Kind: KeyLeft}, nil
	case 'H':
		return Key{Kind: KeyHome}, nil
	case 'F':
		return Key{Kind: KeyEnd}, nil
	case '1', '3', '4', '2':
		next, readErr := d.readByte(ctx)
		if readErr != nil {
			return Key{}, readErr
		}
		if next == '~' {
			switch b {
			case '1':
				return Key{Kind: KeyHome}, nil
			case '3':
				return Key{Kind: KeyDelete}, nil
			case '4':
				return Key{Kind: KeyEnd}, nil
			}
		}
		if b == '2' && next == '0' {
			third, readErr := d.readByte(ctx)
			if readErr != nil {
				return Key{}, readErr
			}
			if third == '0' {
				fourth, readErr := d.readByte(ctx)
				if readErr != nil {
					return Key{}, readErr
				}
				if fourth == '~' {
					return d.readPaste(ctx)
				}
			}
		}
	}
	return Key{Kind: KeyEscape}, nil
}

func (d *KeyDecoder) readPaste(ctx context.Context) (Key, error) {
	const end = "\x1b[201~"
	data := make([]byte, 0, 256)
	for {
		b, err := d.readByte(ctx)
		if err != nil {
			return Key{}, fmt.Errorf("terminal: unterminated bracketed paste: %w", err)
		}
		data = append(data, b)
		if len(data) > d.maxPaste+len(end) {
			return Key{}, errors.New("terminal: bracketed paste exceeds byte limit")
		}
		if len(data) >= len(end) && string(data[len(data)-len(end):]) == end {
			data = data[:len(data)-len(end)]
			break
		}
	}
	if !utf8.Valid(data) {
		return Key{}, errors.New("terminal: bracketed paste is not valid UTF-8")
	}
	for _, r := range string(data) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' {
			return Key{}, fmt.Errorf("terminal: rejected pasted control character %U", r)
		}
	}
	return Key{Kind: KeyPaste, Text: string(data)}, nil
}

func (d *KeyDecoder) readRune(ctx context.Context, first byte) (rune, error) {
	width := 0
	switch {
	case first&0xe0 == 0xc0:
		width = 2
	case first&0xf0 == 0xe0:
		width = 3
	case first&0xf8 == 0xf0:
		width = 4
	default:
		return 0, errors.New("terminal: invalid UTF-8 input")
	}
	buf := []byte{first}
	for len(buf) < width {
		b, err := d.readByte(ctx)
		if err != nil {
			return 0, fmt.Errorf("terminal: incomplete UTF-8 input: %w", err)
		}
		buf = append(buf, b)
	}
	r, n := utf8.DecodeRune(buf)
	if r == utf8.RuneError || n != width {
		return 0, errors.New("terminal: invalid UTF-8 input")
	}
	return r, nil
}

func (d *KeyDecoder) readByte(ctx context.Context) (byte, error) {
	for {
		b, ok, err := d.readByteOnce(ctx)
		if err != nil {
			return 0, err
		}
		if ok {
			return b, nil
		}
	}
}

func (d *KeyDecoder) readByteOnce(ctx context.Context) (byte, bool, error) {
	if len(d.pending) == 0 {
		if d.readErr != nil {
			err := d.readErr
			d.readErr = nil
			return 0, false, err
		}
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		buf := make([]byte, 256)
		var n int
		var err error
		if reader, ok := d.reader.(ContextReader); ok {
			n, err = reader.ReadContext(ctx, buf)
		} else {
			n, err = d.reader.Read(buf)
		}
		if n > 0 {
			d.pending = append(d.pending, buf[:n]...)
			d.readErr = err
		} else if errors.Is(err, io.EOF) && d.timedTTY {
			// VMIN=0/VTIME>0 terminal reads may surface an idle timeout as EOF.
			// Ctrl-D is still delivered as byte 0x04 in raw mode.
			return 0, false, nil
		} else if err != nil {
			return 0, false, err
		} else {
			return 0, false, nil
		}
	}
	b := d.pending[0]
	d.pending = d.pending[1:]
	return b, true, nil
}

func (d *KeyDecoder) unreadByte(b byte) { d.pending = append([]byte{b}, d.pending...) }
