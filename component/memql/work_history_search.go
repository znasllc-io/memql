package memql

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	workHistorySearchDepth = 4
	workHistorySearchBytes = 8 << 20
)

// Search human text in JSON strings, not their escape sequences. Complete JSON
// objects/arrays inside a string are tool envelopes; descend into their strings
// too. Prose merely containing escape-looking bytes is never unescaped. The
// answer remains a byte position in the ORIGINAL serialized message, so page
// offsets and exact reconstruction do not change. Keys remain searchable;
// cross-token JSON syntax and nesting beyond the bounds are not text matches.
func workHistorySearchPosition(source, search string) int {
	remaining := workHistorySearchBytes
	return workHistorySearchJSON(source, strings.ToLower(search), 0, &remaining)
}

func workHistorySearchJSON(source, search string, depth int, remaining *int) int {
	if depth >= workHistorySearchDepth || len(source) > *remaining {
		return -1
	}
	*remaining -= len(source)
	if !json.Valid([]byte(source)) {
		return -1
	}
	for start := 0; start < len(source); start++ {
		if source[start] != '"' {
			continue
		}
		end := start + 1
		for ; end < len(source); end++ {
			if source[end] == '\\' {
				end++ // A quoted backslash/quote is part of this string.
			} else if source[end] == '"' {
				break
			}
		}
		var value string
		if json.Unmarshal([]byte(source[start:end+1]), &value) != nil {
			return -1 // json.Valid above makes this unreachable.
		}
		following := strings.TrimLeft(source[end+1:], " \t\r\n")
		key := strings.HasPrefix(following, ":")
		trimmed := strings.TrimSpace(value)
		nested := !key && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(value))
		position := -1
		if nested {
			// Do not search the envelope's encoded text as a fallback: a
			// literal \\n must not match its encoding of an actual newline.
			position = workHistorySearchJSON(value, search, depth+1, remaining)
		} else {
			lower := strings.ToLower(value)
			if match := strings.Index(lower, search); match >= 0 {
				// Lowercasing can change UTF-8 widths, but not rune count.
				position = workHistoryRuneByteOffset(value, utf8.RuneCountInString(lower[:match]))
			}
		}
		if position >= 0 {
			runes := utf8.RuneCountInString(value[:position])
			return start + workHistoryJSONRuneOffset(source[start:end+1], runes)
		}
		start = end
	}
	return -1
}

func workHistoryRuneByteOffset(value string, count int) int {
	position := 0
	for ; count > 0; count-- {
		_, size := utf8.DecodeRuneInString(value[position:])
		position += size
	}
	return position
}

// literal is a valid JSON string, including quotes. Map a decoded rune offset
// back through escapes without rewriting or normalizing any source bytes.
func workHistoryJSONRuneOffset(literal string, count int) int {
	position := 1
	for ; count > 0; count-- {
		if literal[position] != '\\' {
			_, size := utf8.DecodeRuneInString(literal[position:])
			position += size
			continue
		}
		if literal[position+1] != 'u' {
			position += 2
			continue
		}
		first, _ := strconv.ParseUint(literal[position+2:position+6], 16, 16)
		position += 6
		if first >= 0xd800 && first <= 0xdbff && position+6 <= len(literal) && literal[position:position+2] == `\u` {
			second, _ := strconv.ParseUint(literal[position+2:position+6], 16, 16)
			if utf16.DecodeRune(rune(first), rune(second)) != utf8.RuneError {
				position += 6
			}
		}
	}
	return position
}
