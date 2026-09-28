package docsmd

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// DefaultDescriptionMax is the length a derived description is cut to: about
// what a search result shows of a meta description.
const DefaultDescriptionMax = 160

// metadataLine matches a bold-lead metadata line, `**Status:** stable` or
// `**Last updated**: 2026-04-25`: a page's bookkeeping, not what it is about.
var metadataLine = regexp.MustCompile(`^\*\*[^*]{1,48}(:\*\*|\*\*:)`)

// horizontalRule matches a thematic break, `---`, `***` or `___`.
var horizontalRule = regexp.MustCompile(`^((-[ \t]*){3,}|(\*[ \t]*){3,}|(_[ \t]*){3,})$`)

// listItem matches a bullet or numbered list item.
var listItem = regexp.MustCompile(`^([-*+]|\d+[.)])([ \t]|$)`)

var (
	inlineImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	inlineLinkT = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	refLink     = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)
	codeSpan    = regexp.MustCompile("`+([^`]*)`+")
	strong      = regexp.MustCompile(`(\*\*|__)([^*_]+)(\*\*|__)`)
	emphasis    = regexp.MustCompile(`(^|[^\w*])[*_]([^*_\s][^*_]*)[*_]`)
	spaces      = regexp.MustCompile(`\s+`)
)

// Description derives a page's one-paragraph description from its body (the
// page without its front matter): the first paragraph of prose, its wrapped
// lines joined, inline markdown reduced to its text, cut to at most max
// characters.
//
// Skipped on the way to it: headings (the H1 included), blank lines,
// bold-lead metadata lines (`**Status:**`, `**Last updated:**`), blockquotes,
// lists, tables, fenced code, HTML, images and thematic breaks. The paragraph
// ends at a blank line or at the first line that is not prose, so a sentence
// wrapped across lines is read whole -- the site's first-line reading cut
// every hard-wrapped opening mid-sentence.
//
// The cut prefers the end of a sentence in the second half of the budget, and
// otherwise ends on a word boundary with an ellipsis. An empty result means
// the page opens with no prose paragraph at all.
func Description(body string, max int) string {
	if max <= 0 {
		max = DefaultDescriptionMax
	}
	var para []string
	fence := ""
	for _, raw := range splitLines(body) {
		line, _ := lineBody(raw)
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if fenceCloses(line, fence) {
				fence = ""
			}
			continue
		}
		if marker := fenceOpens(line); marker != "" {
			if len(para) > 0 {
				break
			}
			fence = marker
			continue
		}
		if trimmed == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if !isProse(trimmed) {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, trimmed)
	}
	return cutDescription(plainText(strings.Join(para, " ")), max)
}

// isProse reports whether a trimmed line can open or continue a paragraph.
func isProse(line string) bool {
	switch {
	case strings.HasPrefix(line, "#"),
		strings.HasPrefix(line, ">"),
		strings.HasPrefix(line, "|"),
		strings.HasPrefix(line, "<"),
		strings.HasPrefix(line, "!["),
		metadataLine.MatchString(line),
		horizontalRule.MatchString(line),
		listItem.MatchString(line):
		return false
	}
	return true
}

// plainText reduces inline markdown to its text.
func plainText(s string) string {
	s = inlineImage.ReplaceAllString(s, "")
	s = inlineLinkT.ReplaceAllString(s, "$1")
	s = refLink.ReplaceAllString(s, "$1")
	s = codeSpan.ReplaceAllString(s, "$1")
	s = strong.ReplaceAllString(s, "$2")
	s = emphasis.ReplaceAllString(s, "$1$2")
	s = strings.ReplaceAll(s, `\`, "")
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// cutDescription shortens s to at most max characters.
func cutDescription(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	head := string(runes[:max])
	// The last sentence end in the second half of the budget.
	if i := strings.LastIndex(head, ". "); i >= 0 && utf8.RuneCountInString(head[:i]) >= max/2 {
		return head[:i+1]
	}
	if strings.HasSuffix(head, ".") && utf8.RuneCountInString(head) >= max/2 {
		return head
	}
	// Otherwise a word boundary, leaving room for the ellipsis.
	head = string(runes[:max-1])
	if i := strings.LastIndex(head, " "); i > 0 {
		head = head[:i]
	}
	return strings.TrimRight(head, " ,;:-") + "…"
}
