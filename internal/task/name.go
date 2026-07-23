package task

import (
	"fmt"
	"strings"
)

// SanitizeName normalizes a friendly task name to lowercase ASCII words
// separated by single hyphens.
func SanitizeName(name string) string {
	var result strings.Builder
	hyphen := true
	for _, char := range strings.ToLower(name) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			result.WriteRune(char)
			hyphen = false
		} else if !hyphen {
			result.WriteByte('-')
			hyphen = true
		}
	}
	return strings.TrimSuffix(result.String(), "-")
}

// UniqueName suffixes a name until available reports that it is unused.
func UniqueName(name string, available func(string) bool) string {
	base := name
	for suffix := 2; !available(name); suffix++ {
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	return name
}
