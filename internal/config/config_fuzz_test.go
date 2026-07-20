package config

import "testing"

func FuzzParseYAML(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{}`),
		[]byte("# comment\nmodel: local/model"),
		[]byte("model: a\nmodel: b"),
		[]byte(`[]`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = Parse(input)
	})
}
