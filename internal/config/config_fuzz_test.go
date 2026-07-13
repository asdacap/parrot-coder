package config

import "testing"

func FuzzParseJSONC(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{}`),
		[]byte("{\n// comment\n\"model\":\"local/model\",\n}"),
		[]byte(`{"model":"a","model":"b"}`),
		[]byte(`[]`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = Parse(input)
	})
}
