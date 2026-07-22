package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// CanonicalJSON returns a stable re-encoding of one JSON value, rejecting empty
// input and trailing data. Tool input is canonicalized before it is stored on a
// plan or shown in a permission prompt.
func CanonicalJSON(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return json.Marshal(value)
}
