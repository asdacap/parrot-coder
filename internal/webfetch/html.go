package webfetch

import (
	"html"
	"strings"
	"unicode"
)

var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "div": true, "dl": true, "fieldset": true, "figcaption": true,
	"figure": true, "footer": true, "form": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "header": true,
	"hr": true, "li": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tr": true, "ul": true,
}

// SanitizeHTML extracts conservative readable text without executing or retaining markup.
func SanitizeHTML(input string) string {
	var output strings.Builder
	hidden := ""
	for i := 0; i < len(input); {
		if input[i] != '<' {
			next := strings.IndexByte(input[i:], '<')
			if next < 0 {
				next = len(input) - i
			}
			if hidden == "" {
				output.WriteString(input[i : i+next])
			}
			i += next
			continue
		}
		if strings.HasPrefix(input[i:], "<!--") {
			end := strings.Index(input[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		end := tagEnd(input, i+1)
		if end < 0 {
			if hidden == "" {
				output.WriteString(input[i:])
			}
			break
		}
		tag, closing := tagName(input[i+1 : end])
		if tag == "script" || tag == "style" || tag == "noscript" || tag == "template" {
			if closing && hidden == tag {
				hidden = ""
			} else if !closing && hidden == "" {
				hidden = tag
			}
		}
		if hidden == "" && blockTags[tag] {
			output.WriteByte('\n')
		}
		i = end + 1
	}
	return normalizeText(html.UnescapeString(stripControls(output.String())))
}

func tagEnd(input string, start int) int {
	var quote byte
	for i := start; i < len(input); i++ {
		if quote != 0 {
			if input[i] == quote {
				quote = 0
			}
			continue
		}
		switch input[i] {
		case '\'', '"':
			quote = input[i]
		case '>':
			return i
		}
	}
	return -1
}

func tagName(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "!") || strings.HasPrefix(raw, "?") {
		return "", false
	}
	closing := raw[0] == '/'
	if closing {
		raw = strings.TrimSpace(raw[1:])
	}
	end := 0
	for end < len(raw) {
		r := rune(raw[end])
		if !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			break
		}
		end++
	}
	return strings.ToLower(raw[:end]), closing
}

func normalizeText(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", "\n"), "\n")
	result := make([]string, 0, len(lines))
	blank := true
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				result = append(result, "")
				blank = true
			}
			continue
		}
		result = append(result, line)
		blank = false
	}
	for len(result) > 0 && result[len(result)-1] == "" {
		result = result[:len(result)-1]
	}
	return strings.Join(result, "\n")
}
