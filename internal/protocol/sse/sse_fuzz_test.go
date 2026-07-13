package sse

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func FuzzDecoder(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("data: {}\n\n"),
		[]byte(": heartbeat\r\nid: 1\r\nevent: update\r\ndata: first\r\ndata: second\r\n\r\n"),
		[]byte("data: unterminated"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		decoder := NewDecoder(bytes.NewReader(input), 64<<10)
		defer decoder.Close()
		for range 64 {
			if _, err := decoder.Next(context.Background()); err != nil {
				if err != io.EOF {
					return
				}
				return
			}
		}
	})
}
