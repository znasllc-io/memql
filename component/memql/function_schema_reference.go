package memql

import (
	"strings"
)

// functionSchemaReferenceExcerpt previously returned a curated
// excerpt from dsl/v1/queries/_querySchemaReference.memql. Pass 3
// of the DSL restructure retired the legacy schema reference file
// alongside the legacy tree. The function stays as an empty stub;
// agents that called this through describeFunction() get an empty
// excerpt and the rest of describeFunction() still works.
func functionSchemaReferenceExcerpt() string {
	return ""
}

func extractSchemaReferenceExcerpt(content string) string {
	lines := strings.Split(content, "\n")
	var out []string

	// Capture a small set of sections by simple markers.
	// 1) SYNTAX examples
	out = append(out, pickSection(lines, "SYNTAX:", 8)...)
	// 2) Mutation restriction note (\"Mutation function\" section contains the critical constraints)
	out = append(out, "")
	out = append(out, pickBetween(lines, "Mutation function (implicit return", "// Automation function", 18)...)

	// Clean up.
	joined := strings.TrimSpace(strings.Join(out, "\n"))
	joined = strings.TrimSpace(joined)
	return joined
}

// pickSection finds a line containing marker and returns that line plus nextN lines.
func pickSection(lines []string, marker string, nextN int) []string {
	for i, line := range lines {
		if strings.Contains(line, marker) {
			end := i + 1 + nextN
			if end > len(lines) {
				end = len(lines)
			}
			return trimTrailingEmpty(lines[i:end])
		}
	}
	return nil
}

// pickBetween returns a slice between a line containing startMarker and a line containing endMarker.
func pickBetween(lines []string, startMarker, endMarker string, maxLines int) []string {
	start := -1
	for i, line := range lines {
		if strings.Contains(line, startMarker) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.Contains(lines[i], endMarker) {
			end = i
			break
		}
	}
	segment := lines[start:end]
	if maxLines > 0 && len(segment) > maxLines {
		segment = segment[:maxLines]
	}
	return trimTrailingEmpty(segment)
}

func trimTrailingEmpty(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
