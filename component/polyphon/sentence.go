package polyphon

import (
	"strings"
	"unicode"
)

// Common abbreviations that should not trigger sentence splits.
var abbreviations = map[string]bool{
	"mr":     true,
	"mrs":    true,
	"ms":     true,
	"dr":     true,
	"prof":   true,
	"sr":     true,
	"jr":     true,
	"st":     true,
	"vs":     true,
	"etc":    true,
	"inc":    true,
	"ltd":    true,
	"corp":   true,
	"dept":   true,
	"gen":    true,
	"gov":    true,
	"sgt":    true,
	"cpl":    true,
	"pvt":    true,
	"capt":   true,
	"col":    true,
	"maj":    true,
	"rev":    true,
	"hon":    true,
	"pres":   true,
	"avg":    true,
	"approx": true,
	// Latin abbreviations
	"e.g": true,
	"i.e": true,
	"cf":  true,
}

// SplitSentences splits text into sentences at natural boundaries.
//
// Rules:
//   - Split on . ! ? followed by whitespace or end-of-string
//   - Don't split abbreviations (Mr., Dr., e.g., i.e., etc.)
//   - Don't split decimal numbers (3.14, $5.99)
//   - Don't split inside quoted text
//   - Merge very short sentences (<20 chars) with the next sentence
//   - Flush remaining text as final sentence (even without punctuation)
func SplitSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	runes := []rune(text)
	n := len(runes)

	var sentences []string
	start := 0
	inQuote := false

	for i := 0; i < n; i++ {
		ch := runes[i]

		// Track quoted regions -- don't split inside them.
		if ch == '"' || ch == '\u201C' || ch == '\u201D' {
			inQuote = !inQuote
			continue
		}

		if inQuote {
			continue
		}

		// Check for sentence-ending punctuation.
		if ch != '.' && ch != '!' && ch != '?' {
			continue
		}

		// The punctuation must be followed by whitespace, end-of-string,
		// or a quote character to count as a sentence boundary.
		atEnd := i == n-1
		followedBySpace := !atEnd && (unicode.IsSpace(runes[i+1]) || runes[i+1] == '"' || runes[i+1] == '\u201C')
		if !atEnd && !followedBySpace {
			continue
		}

		// For periods, check if this is an abbreviation or decimal number.
		if ch == '.' {
			if isAbbreviationOrNumber(runes, i) {
				continue
			}
		}

		// Handle consecutive punctuation (e.g., "..." or "?!" or "!!!")
		end := i + 1
		for end < n && (runes[end] == '.' || runes[end] == '!' || runes[end] == '?') {
			end++
		}

		sentence := strings.TrimSpace(string(runes[start:end]))
		if sentence != "" {
			sentences = append(sentences, sentence)
		}
		// Skip any whitespace after the punctuation.
		for end < n && unicode.IsSpace(runes[end]) {
			end++
		}
		start = end
		i = end - 1 // -1 because loop increments
	}

	// Flush remaining text as the final sentence.
	if start < n {
		remainder := strings.TrimSpace(string(runes[start:]))
		if remainder != "" {
			sentences = append(sentences, remainder)
		}
	}

	// Merge very short sentences (<10 chars) with the next one.
	// This prevents choppy TTS for fragments like "Hi.", "Oh.", "Yes."
	// while keeping normal short sentences like "Hello world." separate.
	sentences = mergeShortSentences(sentences, 10)

	return sentences
}

// isAbbreviationOrNumber returns true if the period at position i
// is part of an abbreviation (Mr., Dr., e.g.) or a decimal number (3.14).
func isAbbreviationOrNumber(runes []rune, i int) bool {
	// Check for decimal numbers: digit before and after the period.
	if i > 0 && i < len(runes)-1 {
		if unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
			return true
		}
	}

	// Check for abbreviations: extract the word before the period.
	wordStart := i - 1
	for wordStart >= 0 && (unicode.IsLetter(runes[wordStart]) || runes[wordStart] == '.') {
		wordStart--
	}
	wordStart++ // move past the non-letter character

	if wordStart < i {
		word := strings.ToLower(string(runes[wordStart:i]))
		if abbreviations[word] {
			return true
		}
	}

	// Single-letter abbreviations followed by period (e.g., "U.S.A.", "J.K.")
	if i > 0 && unicode.IsUpper(runes[i-1]) {
		// Check if it's a single uppercase letter before the period.
		if i-1 == 0 || !unicode.IsLetter(runes[i-2]) || runes[i-2] == '.' {
			return true
		}
	}

	return false
}

// mergeShortSentences merges sentences shorter than minLen characters
// with the following sentence.
func mergeShortSentences(sentences []string, minLen int) []string {
	if len(sentences) <= 1 {
		return sentences
	}

	var merged []string
	carry := ""

	for _, s := range sentences {
		if carry != "" {
			s = carry + " " + s
			carry = ""
		}

		if len(s) < minLen {
			carry = s
		} else {
			merged = append(merged, s)
		}
	}

	// If there's leftover carry, append it to the last sentence or add standalone.
	if carry != "" {
		if len(merged) > 0 {
			merged[len(merged)-1] = merged[len(merged)-1] + " " + carry
		} else {
			merged = append(merged, carry)
		}
	}

	return merged
}
