// Package id creates lexicographically sortable, prefixed identifiers.
package id

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	encodedLength = 26
	alphabet      = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// New returns an ID containing a millisecond timestamp and 80 bits of
// cryptographic randomness. The timestamp makes IDs sortable; randomness makes
// IDs generated in the same millisecond safe across processes.
func New(prefix string) (string, error) {
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	var raw [16]byte
	milliseconds := uint64(time.Now().UnixMilli())
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	raw[2] = byte(milliseconds >> 24)
	raw[3] = byte(milliseconds >> 16)
	raw[4] = byte(milliseconds >> 8)
	raw[5] = byte(milliseconds)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("generate random ID: %w", err)
	}
	return prefix + "_" + encode(raw), nil
}

func encode(raw [16]byte) string {
	var out [encodedLength]byte
	// A ULID is 128 bits encoded into 26 base32 characters. The first character
	// carries only three bits, preventing overflow and preserving byte ordering.
	out[0] = alphabet[raw[0]>>5]
	var accumulator uint32 = uint32(raw[0] & 0x1f)
	bits := 5
	input := 1
	for output := 1; output < len(out); output++ {
		for bits < 5 {
			accumulator = accumulator<<8 | uint32(raw[input])
			input++
			bits += 8
		}
		bits -= 5
		out[output] = alphabet[(accumulator>>bits)&0x1f]
		accumulator &= (1 << bits) - 1
	}
	return string(out[:])
}

// Validate checks the complete syntax and canonical encoding of an ID.
func Validate(value string) error {
	separator := strings.LastIndexByte(value, '_')
	if separator < 1 {
		return errors.New("ID must contain a prefix and underscore")
	}
	if err := validatePrefix(value[:separator]); err != nil {
		return err
	}
	encoded := value[separator+1:]
	if len(encoded) != encodedLength {
		return fmt.Errorf("ID payload must be %d characters", encodedLength)
	}
	if encoded[0] > '7' {
		return errors.New("ID payload overflows 128 bits")
	}
	for _, char := range encoded {
		if !strings.ContainsRune(alphabet, char) {
			return fmt.Errorf("invalid ID character %q", char)
		}
	}
	return nil
}

// ValidatePrefix additionally requires the expected prefix.
func ValidatePrefix(value, prefix string) error {
	if err := Validate(value); err != nil {
		return err
	}
	if !strings.HasPrefix(value, prefix+"_") {
		return fmt.Errorf("ID does not have prefix %q", prefix)
	}
	return nil
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return errors.New("ID prefix is empty")
	}
	for _, char := range prefix {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return errors.New("ID prefix must contain only lowercase ASCII letters and digits")
		}
	}
	return nil
}
