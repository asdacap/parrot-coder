package tool

import (
	"bufio"
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

func readBoundedLine(r *bufio.Reader, max int64) (string, error) {
	var b strings.Builder
	for {
		fragment, prefix, err := r.ReadLine()
		if int64(b.Len()+len(fragment)+1) > max {
			return "", errors.New("line byte limit exceeded")
		}
		b.Write(fragment)
		if !prefix {
			if err == nil {
				b.WriteByte('\n')
			}
			return b.String(), err
		}
		if err != nil {
			return b.String(), err
		}
	}
}
func isBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b)
}
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || strings.IndexByte(pattern, 0) >= 0 || filepath.IsAbs(pattern) {
		return nil, errors.New("invalid glob pattern")
	}
	clean := filepath.ToSlash(filepath.Clean(pattern))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, errors.New("glob traversal is not allowed")
	}
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(clean); {
		switch clean[i] {
		case '*':
			if i+1 < len(clean) && clean[i+1] == '*' {
				if i+2 < len(clean) && clean[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(clean[i : i+1]))
			i++
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}
