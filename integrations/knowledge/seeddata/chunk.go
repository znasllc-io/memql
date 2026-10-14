package seeddata

import "strings"

// Chunk retains the canonical ingestion chunking contract.
func Chunk(text string, size, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if size <= 0 {
		size = 1800
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}
	if len(text) <= size {
		return []string{text}
	}

	runes := []rune(text)
	var chunks []string
	i := 0
	for i < len(runes) {
		end := i + size
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimSpace(string(runes[i:])))
			break
		}
		// Prefer a paragraph break within the last 25% of the window.
		window := string(runes[i:end])
		cut := end
		if idx := strings.LastIndex(window, "\n\n"); idx > size*3/4 {
			cut = i + idx + 2
		} else if idx := strings.LastIndex(window, ". "); idx > size*3/4 {
			cut = i + idx + 2
		} else if idx := strings.LastIndex(window, "\n"); idx > size*3/4 {
			cut = i + idx + 1
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[i:cut])))
		next := cut - overlap
		if next <= i {
			next = i + 1
		}
		i = next
	}
	return chunks
}
