package docsmd

import (
	"regexp"
	"strings"
)

// Link is one markdown link, image or reference definition on a page, found
// outside code and HTML comments. Every offset is a byte offset into the
// content Links was given, so a caller can rewrite the target, or replace the
// whole link with its text, in place.
type Link struct {
	// Start and End bound the whole construct: `[text](target)`, including
	// the leading `!` of an image, or a whole reference-definition line.
	Start, End int
	// TargetStart and TargetEnd bound the target as written.
	TargetStart, TargetEnd int
	Text                   string // the link text, or an image's alt text, as written
	Target                 string // the target as written, fragment included
	Image                  bool
	// Definition marks a reference-style definition, `[label]: target`.
	Definition bool
	Line       int // 1-based line of Start
}

// Path returns the target without its `#fragment`.
func (l Link) Path() string {
	p, _, _ := strings.Cut(l.Target, "#")
	return p
}

// Fragment returns the target's `#...` suffix, or "".
func (l Link) Fragment() string {
	if i := strings.Index(l.Target, "#"); i >= 0 {
		return l.Target[i:]
	}
	return ""
}

// inlineLink matches `[text](target)`: the text holds no `]`, and the target
// no `)` and no whitespace -- the shape the relative-link gate has always
// read, so a target with a literal space is invisible here the same way a
// strict renderer does not make it a link.
var inlineLink = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)

// definition matches a reference-style definition line, `[label]: target`,
// indented at most three spaces. A label opening with `^` is a footnote,
// `[^1]: text`, whose "target" is prose, not a link.
var definition = regexp.MustCompile(`(?m)^ {0,3}\[([^\]^][^\]]*)\]:[ \t]*(\S+)`)

// Links returns every link, image and reference definition in content outside
// fenced code, code spans and HTML comments, in document order.
func Links(content string) []Link {
	masked := Mask(content)
	var links []Link
	for _, m := range inlineLink.FindAllStringSubmatchIndex(masked, -1) {
		l := Link{
			Start:       m[0],
			End:         m[1],
			TargetStart: m[4],
			TargetEnd:   m[5],
			Text:        content[m[2]:m[3]],
			Target:      content[m[4]:m[5]],
		}
		if m[0] > 0 && masked[m[0]-1] == '!' {
			l.Image = true
			l.Start--
		}
		links = append(links, l)
	}
	for _, m := range definition.FindAllStringSubmatchIndex(masked, -1) {
		lineEnd := strings.IndexByte(content[m[0]:], '\n')
		end := len(content)
		if lineEnd >= 0 {
			end = m[0] + lineEnd
		}
		links = append(links, Link{
			Start:       m[0],
			End:         end,
			TargetStart: m[4],
			TargetEnd:   m[5],
			Text:        content[m[2]:m[3]],
			Target:      content[m[4]:m[5]],
			Definition:  true,
		})
	}
	sortLinks(links)
	for i := range links {
		links[i].Line = 1 + strings.Count(content[:links[i].Start], "\n")
	}
	return links
}

func sortLinks(links []Link) {
	// Insertion sort: a page has tens of links, and the two passes are each
	// already in order.
	for i := 1; i < len(links); i++ {
		for j := i; j > 0 && links[j].Start < links[j-1].Start; j-- {
			links[j], links[j-1] = links[j-1], links[j]
		}
	}
}

// scheme matches a URL scheme prefix such as `https:` or `mailto:`.
var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// IsExternal reports whether a link target leaves the repository's files
// altogether: a URL with a scheme (`https:`, `mailto:`), a protocol-relative
// `//host` target, or a same-page `#fragment`. Every other target is a path
// into the tree, relative to the linking file or, with a leading `/`, to the
// repository root.
func IsExternal(target string) bool {
	return strings.HasPrefix(target, "#") ||
		strings.HasPrefix(target, "//") ||
		scheme.MatchString(target) ||
		strings.Contains(target, "://")
}
