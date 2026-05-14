package fileprocessor

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxTextBytes = 500_000 // 500 KB of text is more than enough

// extractText reads plain-text or Markdown file bytes as UTF-8 text.
func extractText(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}

	if len(data) > maxTextBytes {
		data = data[:maxTextBytes]
	}

	if !utf8.Valid(data) {
		return "", fmt.Errorf("file content is not valid UTF-8")
	}

	return strings.TrimSpace(string(data)), nil
}
