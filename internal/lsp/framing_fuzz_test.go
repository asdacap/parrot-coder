package lsp

import (
	"bufio"
	"bytes"
	"testing"
)

func FuzzReadFrame(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("Content-Length: 2\r\n\r\n{}"),
		[]byte("Content-Length: 4\n\nnull"),
		[]byte("Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = readFrame(bufio.NewReader(bytes.NewReader(input)), 64<<10)
	})
}
