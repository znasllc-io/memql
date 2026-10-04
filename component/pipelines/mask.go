package pipelines

import (
	"cmp"
	"slices"
	"strings"
)

// minMaskedSecret is the shortest value MaskSecrets masks. Shorter values
// hide next to nothing and would mangle ordinary output: a secret "1" would
// mask every 1 in a log.
const minMaskedSecret = 4

// secretMask is what a secret's bytes become.
const secretMask = "***"

// MaskSecrets replaces every occurrence of each value (4+ bytes) with ***.
//
// A value is masked in each form it is likely printed in: as stored, without
// the surrounding whitespace a stored value often carries (a trailing
// newline), and line by line when it spans lines (a private key, printed one
// line at a time behind a log prefix). Occurrences that overlap -- of one
// value, or of two -- are masked as one span, so no byte of either survives
// between two masks.
func MaskSecrets(text string, values []string) string {
	needles := maskForms(values)
	if text == "" || len(needles) == 0 {
		return text
	}
	type span struct{ start, end int }
	var spans []span
	for _, needle := range needles {
		for from := 0; from+len(needle) <= len(text); {
			i := strings.Index(text[from:], needle)
			if i < 0 {
				break
			}
			start := from + i
			spans = append(spans, span{start, start + len(needle)})
			from = start + 1 // overlapping occurrences too
		}
	}
	if len(spans) == 0 {
		return text
	}
	slices.SortFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })

	var b strings.Builder
	b.Grow(len(text))
	written := 0
	current := spans[0]
	flush := func() {
		b.WriteString(text[written:current.start])
		b.WriteString(secretMask)
		written = current.end
	}
	for _, s := range spans[1:] {
		if s.start <= current.end {
			current.end = max(current.end, s.end)
			continue
		}
		flush()
		current = s
	}
	flush()
	b.WriteString(text[written:])
	return b.String()
}

// maskForms is every form of every value worth masking, deduplicated.
func maskForms(values []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if len(s) >= minMaskedSecret && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, v := range values {
		add(v)
		add(strings.TrimSpace(v))
		if strings.Contains(v, "\n") {
			for _, line := range strings.Split(v, "\n") {
				add(strings.TrimSpace(line))
			}
		}
	}
	return out
}
