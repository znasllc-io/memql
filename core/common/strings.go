package common

import (
	"strings"
	"unicode"
)

// ToUpperCamel converts an arbitrary identifier into UpperCamelCase (PascalCase).
// Non-alphanumeric characters act as delimiters, and consecutive delimiters are collapsed.
// Returns an empty string when the input has no alphanumeric characters.
func ToUpperCamel(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	split := strings.FieldsFunc(trimmed, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(split) == 0 {
		return ""
	}

	var builder strings.Builder
	for _, part := range split {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		lower := strings.ToLower(part)
		runes := []rune(lower)
		runes[0] = unicode.ToUpper(runes[0])
		builder.WriteString(string(runes))
	}

	return builder.String()
}
