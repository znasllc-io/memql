package env

import (
	"strings"
	"unicode"

	"github.com/visionarys-io/memql/core/common"
)

// EnvPrefixForComponent converts a component name (which may be camelCase, kebab-case, etc.)
// into the canonical screaming snake case prefix used for environment variables.
func EnvPrefixForComponent(component common.ComponentName) string {
	name := strings.TrimSpace(string(component))
	if name == "" {
		return ""
	}

	var (
		builder           strings.Builder
		runes             = []rune(name)
		pendingUnderscore bool
		lastWasUnderscore bool
	)

	writeUnderscore := func() {
		if builder.Len() == 0 || lastWasUnderscore {
			return
		}
		builder.WriteByte('_')
		lastWasUnderscore = true
		pendingUnderscore = false
	}

	writeRune := func(r rune) {
		builder.WriteRune(unicode.ToUpper(r))
		lastWasUnderscore = false
		pendingUnderscore = false
	}

	for i, r := range runes {
		switch {
		case unicode.IsLetter(r):
			if pendingUnderscore || letterBoundary(runes, i) {
				writeUnderscore()
			}
			writeRune(r)
		case unicode.IsDigit(r):
			if pendingUnderscore || digitBoundary(runes, i) {
				writeUnderscore()
			}
			builder.WriteRune(r)
			lastWasUnderscore = false
			pendingUnderscore = false
		default:
			if builder.Len() > 0 {
				pendingUnderscore = true
			}
		}
	}

	return builder.String()
}

func letterBoundary(runes []rune, idx int) bool {
	if idx == 0 {
		return false
	}

	curr := runes[idx]
	prev := runes[idx-1]

	switch {
	case unicode.IsUpper(curr):
		if unicode.IsLower(prev) || unicode.IsDigit(prev) {
			return true
		}
		if unicode.IsUpper(prev) && idx+1 < len(runes) && unicode.IsLower(runes[idx+1]) {
			return true
		}
	case unicode.IsLower(curr):
		return unicode.IsDigit(prev)
	}

	return false
}

func digitBoundary(runes []rune, idx int) bool {
	if idx == 0 {
		return false
	}
	return unicode.IsLetter(runes[idx-1])
}
