package docsmd

import (
	"regexp"
	"sort"
	"strings"
)

// Link is one markdown link, image or reference definition on a page, found
// outside code and HTML comments. Every offset is a byte offset into the
// content Links was given, so a caller can rewrite the target, or unwrap the
// link to its text, in place.
type Link struct {
	// Start and End bound the whole construct: `[text](target "title")`,
	// including the leading `!` of an image, or a reference definition from
	// its `[` to the end of its last line.
	Start, End int
	// TextStart and TextEnd bound the link text, an image's alt text, or a
	// definition's label, as written. Removing Start..TextStart and
	// TextEnd..End unwraps a link to its text and keeps any link or image
	// nested in that text where a caller can still rewrite it.
	TextStart, TextEnd int
	// TargetStart and TargetEnd bound the destination as written, inside its
	// angle brackets when it has them, so writing a new target there keeps
	// the link valid.
	TargetStart, TargetEnd int
	Text                   string // the link text, alt text or label, as written
	Target                 string // the destination as written, fragment included
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

// Links returns every inline link, image and reference definition in content
// outside code and HTML comments, in document order: a link and an image
// nested in its text are both returned, the outer one first.
//
// Inline links are read the way CommonMark reads them (scan.go): a title
// after the destination ("...", '...' or (...)), whitespace around both, an
// angle-bracket destination, brackets and images inside the link text, and
// text across lines. Reference-style uses (`[text][label]`, `[label]`) are
// not returned: their target is their definition's, which is.
func Links(content string) []Link {
	r := read(content)
	links := append([]Link(nil), r.links...)
	links = append(links, definitions(content, r.mask(content))...)
	sort.SliceStable(links, func(i, j int) bool { return links[i].Start < links[j].Start })
	for i := range links {
		links[i].Line = 1 + strings.Count(content[:links[i].Start], "\n")
	}
	return links
}

// definitions returns the reference definitions, `[label]: target "title"`,
// that open a line of masked, so none inside code or a comment. A label
// opening with `^` is a footnote, `[^1]: text`, whose "target" is prose.
//
// A definition may sit inside a block quote or a list item, and its target
// and title may each follow on the next line. A definition-shaped line that
// CommonMark would read as paragraph text is returned too: checking a target
// that is not a link fails closed, where missing one would ship it.
func definitions(content, masked string) []Link {
	var out []Link
	off := 0
	for _, raw := range splitLines(masked) {
		if l, ok := definitionAt(content, masked, off); ok {
			out = append(out, l)
		}
		off += len(raw)
	}
	return out
}

func definitionAt(content, masked string, ls int) (Link, bool) {
	n := len(masked)
	p := ls
	// The line's container markers: indentation, block-quote markers, list
	// item markers.
	for {
		q := skipSpaceTab(masked, p, n)
		if q < n && masked[q] == '>' {
			p = q + 1
			continue
		}
		if m := listMarker.FindStringIndex(masked[q:min(n, q+12)]); m != nil {
			p = q + m[1]
			continue
		}
		p = q
		break
	}
	if p >= n || masked[p] != '[' || p+1 < n && masked[p+1] == '^' {
		return Link{}, false
	}
	j := p + 1
	for ; j < n; j++ {
		c := masked[j]
		if c == '\\' && j+1 < n && isPunct(masked[j+1]) {
			j++
			continue
		}
		if c == '[' || c == ']' {
			break
		}
		if c == '\n' && blankLineAt(masked, j+1) {
			return Link{}, false
		}
	}
	if j+1 >= n || masked[j] != ']' || masked[j+1] != ':' || isBlank(masked[p+1:j]) {
		return Link{}, false
	}
	i := skipSpaceNewline(masked, j+2, n)
	start, end, next, ok := parseDestination(masked, i, n)
	if !ok || start == end && masked[i] != '<' {
		return Link{}, false
	}
	last := next
	if k := skipSpaceNewline(masked, next, n); k > next && k < n && strings.IndexByte(`"'(`, masked[k]) >= 0 {
		if after, ok := titleEnd(masked, k, n); ok && isBlank(masked[after:lineEnd(masked, after)]) {
			last = after
		}
	}
	return Link{
		Start: p, End: lineEnd(masked, last),
		TextStart: p + 1, TextEnd: j,
		TargetStart: start, TargetEnd: end,
		Text:       content[p+1 : j],
		Target:     content[start:end],
		Definition: true,
	}, true
}

// listMarker matches a list item marker and the whitespace after it.
var listMarker = regexp.MustCompile(`^([-*+]|\d{1,9}[.)])[ \t]+`)

// lineEnd returns the offset of the end of the line holding offset i,
// before its line ending.
func lineEnd(s string, i int) int {
	e := strings.IndexByte(s[i:], '\n')
	if e < 0 {
		return len(s)
	}
	e += i
	if e > i && s[e-1] == '\r' {
		e--
	}
	return e
}

// blankLineAt reports whether the line starting at offset i is blank.
func blankLineAt(s string, i int) bool {
	return isBlank(s[i:lineEnd(s, i)])
}

// scheme matches a URL scheme prefix such as `https:` or `mailto:`.
var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// IsExternal reports whether a link target leaves the repository's files
// altogether: a URL with a scheme (`https:`, `mailto:`), a protocol-relative
// `//host` target, or the page itself -- a `#fragment`, or an empty
// destination, `[text]()`. Every other target is a path into the tree,
// relative to the linking file or, with a leading `/`, to the repository
// root.
func IsExternal(target string) bool {
	return target == "" ||
		strings.HasPrefix(target, "#") ||
		strings.HasPrefix(target, "//") ||
		scheme.MatchString(target) ||
		strings.Contains(target, "://")
}
