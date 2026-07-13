package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxFrameHeaderBytes = 8 << 10

// FramedReader reads Content-Length framed JSON messages.
type FramedReader struct {
	reader *bufio.Reader
	max    int64
}

func NewFramedReader(reader io.Reader, maxMessageBytes int64) *FramedReader {
	if maxMessageBytes <= 0 {
		maxMessageBytes = defaultMessageBytes
	}
	return &FramedReader{reader: bufio.NewReaderSize(reader, 4096), max: maxMessageBytes}
}

func (r *FramedReader) Read() (json.RawMessage, error) {
	contentLength := int64(-1)
	headerBytes := 0
	for {
		line, err := r.readHeaderLine(maxFrameHeaderBytes - headerBytes)
		headerBytes += len(line)
		if headerBytes > maxFrameHeaderBytes {
			return nil, errors.New("mcp: frame headers exceed byte limit")
		}
		if err != nil {
			if errors.Is(err, io.EOF) && headerBytes == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("mcp: read frame header: %w", err)
		}
		if line == "\n" || line == "\r\n" {
			break
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("mcp: malformed frame header")
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			if contentLength >= 0 {
				return nil, errors.New("mcp: duplicate Content-Length header")
			}
			value = strings.TrimSpace(value)
			if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
				return nil, errors.New("mcp: invalid Content-Length header")
			}
			n, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || n < 0 {
				return nil, errors.New("mcp: invalid Content-Length header")
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, errors.New("mcp: missing Content-Length header")
	}
	if contentLength > r.max {
		return nil, fmt.Errorf("mcp: frame size %d exceeds limit %d", contentLength, r.max)
	}
	body := make([]byte, int(contentLength))
	if _, err := io.ReadFull(r.reader, body); err != nil {
		return nil, fmt.Errorf("mcp: read frame body: %w", err)
	}
	if !json.Valid(body) {
		return nil, errors.New("mcp: frame body is not valid JSON")
	}
	return json.RawMessage(body), nil
}

func (r *FramedReader) readHeaderLine(remaining int) (string, error) {
	if remaining <= 0 {
		return "", errors.New("mcp: frame headers exceed byte limit")
	}
	var output []byte
	for {
		part, err := r.reader.ReadSlice('\n')
		output = append(output, part...)
		if len(output) > remaining {
			return "", errors.New("mcp: frame headers exceed byte limit")
		}
		if err == nil {
			return string(output), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(output), err
		}
	}
}

// FramedWriter writes Content-Length framed JSON messages safely from
// concurrent goroutines.
type FramedWriter struct {
	writer io.Writer
	max    int64
	gate   chan struct{}
}

func NewFramedWriter(writer io.Writer, maxMessageBytes int64) *FramedWriter {
	if maxMessageBytes <= 0 {
		maxMessageBytes = defaultMessageBytes
	}
	return &FramedWriter{writer: writer, max: maxMessageBytes, gate: make(chan struct{}, 1)}
}

func (w *FramedWriter) Write(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("mcp: encode frame: %w", err)
	}
	_, err = w.writeBody(body)
	return err
}

func (w *FramedWriter) writeBody(body []byte) (int, error) {
	return w.writeBodyContext(context.Background(), body)
}

func (w *FramedWriter) writeBodyContext(ctx context.Context, body []byte) (int, error) {
	if !json.Valid(body) {
		return 0, errors.New("mcp: refusing to write invalid JSON frame")
	}
	if int64(len(body)) > w.max {
		return 0, errors.New("mcp: outgoing frame exceeds byte limit")
	}
	header := []byte("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
	frame := make([]byte, 0, len(header)+len(body))
	frame = append(frame, header...)
	frame = append(frame, body...)
	select {
	case w.gate <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-w.gate }()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	written := 0
	for len(frame) > 0 {
		n, err := w.writer.Write(frame)
		written += n
		frame = frame[n:]
		if err != nil {
			return written, fmt.Errorf("mcp: write frame: %w", err)
		}
		if n == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

func encodeFrame(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := NewFramedWriter(&output, int64(len(body))).Write(json.RawMessage(body)); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
