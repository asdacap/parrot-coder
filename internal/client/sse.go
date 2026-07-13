package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

type EventStream struct {
	body    io.ReadCloser
	decoder *SSEDecoder
}

func (s *EventStream) Next() (v1.Event, error) { return s.decoder.Next() }
func (s *EventStream) Close() error            { return s.body.Close() }

type SSEDecoder struct {
	reader *bufio.Reader
	max    int
}

func NewSSEDecoder(reader io.Reader, maxEventBytes int) *SSEDecoder {
	if maxEventBytes <= 0 {
		maxEventBytes = 1 << 20
	}
	return &SSEDecoder{reader: bufio.NewReaderSize(reader, 4096), max: maxEventBytes}
}

func (d *SSEDecoder) Next() (v1.Event, error) {
	var id, eventType string
	var data []string
	size := 0
	for {
		line, err := d.readLine(d.max - size)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(data) == 0 {
					return v1.Event{}, io.EOF
				}
				return decodeSSEEvent(id, eventType, data)
			}
			return v1.Event{}, err
		}
		size += len(line)
		if size > d.max {
			return v1.Event{}, errors.New("client: SSE event exceeds configured limit")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if len(data) == 0 {
				id, eventType, size = "", "", 0
				continue
			}
			return decodeSSEEvent(id, eventType, data)
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "event":
			eventType = value
		case "data":
			data = append(data, value)
		}
	}
}

func decodeSSEEvent(id, eventType string, data []string) (v1.Event, error) {
	var item v1.Event
	decoder := json.NewDecoder(bytes.NewBufferString(strings.Join(data, "\n")))
	if err := decoder.Decode(&item); err != nil {
		return v1.Event{}, errors.New("client: invalid SSE JSON data")
	}
	if item.ID == "" {
		item.ID = id
	}
	if item.Type == "" {
		item.Type = eventType
	}
	if item.ID != id || item.Type != eventType {
		return v1.Event{}, errors.New("client: SSE fields do not match event envelope")
	}
	if _, err := v1.DecodeEventData(item); err != nil {
		return v1.Event{}, errors.New("client: SSE event payload does not match the v1 manifest")
	}
	return item, nil
}

func (d *SSEDecoder) readLine(remaining int) (string, error) {
	if remaining <= 0 {
		return "", errors.New("client: SSE event exceeds configured limit")
	}
	var output []byte
	for {
		part, err := d.reader.ReadSlice('\n')
		output = append(output, part...)
		if len(output) > remaining {
			return "", errors.New("client: SSE event exceeds configured limit")
		}
		if err == nil {
			return string(output), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(output) > 0 {
				return string(output), nil
			}
			return "", err
		}
	}
}
