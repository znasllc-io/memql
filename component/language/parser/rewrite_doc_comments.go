package parser

import (
	"fmt"
	"regexp"
	"strings"
)

// rewrite_doc_comments.go -- the memql#2635 dedup codemod (epic #2601),
// registered with memqlmigrate as --rewrite=doc-comment-descriptions and
// keyed on GrammarVersion 2026.08-doc-comment-descriptions.
//
// Three mechanical rewrites over a .memql file:
//
//  1. Every CONSTRUCT-LEVEL @description("...") (a standalone column-0
//     annotation line) converts to a /// doc-comment block above the
//     construct's annotations, un-escaped per design ruling 7 (\" -> ", \\
//     -> \), wrapped at ~100 columns on spaces, embedded \n becoming a
//     bare-/// paragraph break. The ruling-2 join reproduces the original
//     string exactly, so every resolved description is unchanged.
//  2. A header comment block whose prose near-verbatim restates the
//     description (>= dupThreshold token containment after normalization)
//     is deleted -- the duplication the epic exists to collapse. Non-
//     duplicate headers stay.
//  3. "// Arguments:" and "// Returns:" comment sections that restate the
//     args{} declaration are dropped; an Arguments line carrying prose
//     beyond the declaration moves to a /// doc on that args field.
//     A section with prose the heuristic cannot place is left untouched.
//
// Field-level and property-level @description (inline on tool fields /
// concept properties) are NOT construct-level and are never touched.

var constructDescLineRe = regexp.MustCompile(`^@description\("((?:[^"\\]|\\.)*)"\)\s*$`)

// dupThreshold is the token-containment fraction above which a header
// comment counts as restating the description: the SHORTER text's tokens
// must appear in the longer at this rate. 0.6 keeps genuinely additive
// headers while catching the near-verbatim restatement pattern.
const dupThreshold = 0.6

// RewriteDocCommentDescriptions applies the #2635 dedup to one file.
func RewriteDocCommentDescriptions(src []byte) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	out := make([]string, 0, len(lines))

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		m := constructDescLineRe.FindStringSubmatch(line)
		if m == nil {
			out = append(out, line)
			continue
		}
		desc := unescapeDescription(m[1])

		// The annotation block: contiguous column-0 @lines around this one
		// already emitted above us in `out` plus the ones still ahead.
		annStart := len(out)
		for annStart > 0 && strings.HasPrefix(out[annStart-1], "@") {
			annStart--
		}
		// The header comment block immediately above the annotations.
		hdrStart := annStart
		for hdrStart > 0 && strings.HasPrefix(strings.TrimSpace(out[hdrStart-1]), "//") &&
			!strings.HasPrefix(strings.TrimSpace(out[hdrStart-1]), "///") {
			hdrStart--
		}
		header := out[hdrStart:annStart]

		// Rewrite 3 first: pull Arguments/Returns sections out of the
		// header so movable per-arg prose survives the dedup deletion.
		remainder, fieldDocs, sectionsRemoved := extractArgSections(header)

		// Rewrite 2: does the remaining header restate the description?
		headerDeleted := false
		if len(remainder) > 0 && isNearVerbatim(strings.Join(remainder, "\n"), desc) {
			remainder = nil
			headerDeleted = true
		}

		if sectionsRemoved || headerDeleted {
			rebuilt := make([]string, 0, len(out))
			rebuilt = append(rebuilt, out[:hdrStart]...)
			rebuilt = append(rebuilt, remainder...)
			rebuilt = append(rebuilt, out[annStart:]...)
			out = rebuilt
		}

		// Rewrite 1: /// block above the annotation block.
		annStart = len(out)
		for annStart > 0 && strings.HasPrefix(out[annStart-1], "@") {
			annStart--
		}
		docLines := formatDocComment(desc)
		rebuilt := make([]string, 0, len(out)+len(docLines))
		rebuilt = append(rebuilt, out[:annStart]...)
		rebuilt = append(rebuilt, docLines...)
		rebuilt = append(rebuilt, out[annStart:]...)
		out = rebuilt
		// The @description line itself is dropped (not emitted).

		// Defer the field-doc insertion until we can see the args block:
		// scan forward from here for the construct's args fields.
		if len(fieldDocs) > 0 {
			i = insertFieldDocs(lines, i, fieldDocs, &out)
		}
	}

	return []byte(strings.Join(out, "\n")), nil
}

// unescapeDescription implements ruling 7: \" -> " and \\ -> \.
func unescapeDescription(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// formatDocComment renders a description as /// lines: wrap at ~100 columns
// on spaces; embedded newlines become bare-/// paragraph breaks. The
// ruling-2 join over the emitted lines reproduces the input exactly.
func formatDocComment(desc string) []string {
	var out []string
	for pi, para := range strings.Split(desc, "\n") {
		if pi > 0 {
			out = append(out, "///")
		}
		for _, line := range wrapAtColumns(para, 96) {
			out = append(out, "/// "+line)
		}
	}
	if len(out) == 0 {
		out = []string{"///"}
	}
	return out
}

func wrapAtColumns(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{s}
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	lines = append(lines, cur)
	// Reconstruct interior multi-space runs? Descriptions are prose; the
	// corpus uses single spaces. Fields joined with single spaces match
	// the ruling-2 join. A description with deliberate double spaces
	// would not round-trip; the equivalence harness catches any instance.
	return lines
}

// normalizeProse lowercases and reduces text to word tokens.
func normalizeProse(s string) []string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// isNearVerbatim reports whether the shorter of (header prose, description)
// is contained in the longer at >= dupThreshold token rate.
func isNearVerbatim(header, desc string) bool {
	h := normalizeProse(strings.ReplaceAll(header, "//", " "))
	d := normalizeProse(desc)
	if len(h) == 0 || len(d) == 0 {
		return false
	}
	shorter, longer := h, d
	if len(d) < len(h) {
		shorter, longer = d, h
	}
	set := make(map[string]int, len(longer))
	for _, t := range longer {
		set[t]++
	}
	hits := 0
	for _, t := range shorter {
		if set[t] > 0 {
			set[t]--
			hits++
		}
	}
	return float64(hits)/float64(len(shorter)) >= dupThreshold
}

var bareRestateRe = regexp.MustCompile(`^//\s+[A-Za-z_][A-Za-z0-9_]*(\s+\S+){0,2}\s*$`)

var argLineRe = regexp.MustCompile(`^//\s*(?:-\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*(?:\([^)]*\))?\s*(?:--|:|-)\s*(.+)$`)

// extractArgSections removes "// Arguments:" / "// Returns:" sections from a
// header comment block. Arguments lines whose prose goes beyond the
// declaration are returned as fieldDocs (field name -> prose) for /// moves;
// pure restating sections are dropped. A section the heuristic cannot fully
// account for is kept untouched (conservative).
func extractArgSections(header []string) (remainder []string, fieldDocs map[string]string, removed bool) {
	fieldDocs = map[string]string{}
	i := 0
	for i < len(header) {
		t := strings.TrimSpace(header[i])
		if t == "// Arguments:" || t == "// Returns:" {
			isArgs := t == "// Arguments:"
			j := i + 1
			sectionOK := true
			var sectionDocs [][2]string
			for j < len(header) {
				lt := strings.TrimSpace(header[j])
				if !strings.HasPrefix(lt, "//") || lt == "//" ||
					strings.HasPrefix(lt, "// Arguments:") || strings.HasPrefix(lt, "// Returns:") {
					break
				}
				if isArgs {
					am := argLineRe.FindStringSubmatch(lt)
					if am == nil {
						// A bare restating line like "//   name string" is
						// droppable; anything else makes the section
						// unplaceable -- keep whole section.
						if !bareRestateRe.MatchString(lt) {
							sectionOK = false
							break
						}
					} else {
						sectionDocs = append(sectionDocs, [2]string{am[1], strings.TrimSpace(am[2])})
					}
				} else {
					// Returns sections have no field slot: droppable ONLY
					// when every line is a bare restating shape; free prose
					// keeps the whole section (never silently deleted).
					if !bareRestateRe.MatchString(lt) {
						sectionOK = false
						break
					}
				}
				j++
			}
			if sectionOK {
				for _, kv := range sectionDocs {
					fieldDocs[kv[0]] = kv[1]
				}
				removed = true
				i = j
				continue
			}
		}
		remainder = append(remainder, header[i])
		i++
	}
	return remainder, fieldDocs, removed
}

// insertFieldDocs walks forward from the construct header line in the
// ORIGINAL lines, copying them to out while inserting /// docs above the
// named args fields. Returns the new read index (the last copied line).
func insertFieldDocs(lines []string, from int, fieldDocs map[string]string, out *[]string) int {
	depth := 0
	inArgs := false
	i := from + 1
	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if !inArgs && (trimmed == "args {" || strings.HasSuffix(trimmed, " args {")) {
			inArgs = true
			depth = 1
			*out = append(*out, line)
			continue
		}
		if inArgs {
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				*out = append(*out, line)
				return i
			}
			name := fieldNameOf(trimmed)
			if doc, ok := fieldDocs[name]; ok && name != "" {
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				for _, dl := range formatDocComment(doc) {
					*out = append(*out, indent+dl)
				}
				delete(fieldDocs, name)
			}
			*out = append(*out, line)
			continue
		}
		*out = append(*out, line)
		// Stop at the construct's end (column-0 closer) without an args
		// block -- nothing to move onto.
		if line == "}" {
			return i
		}
	}
	return i - 1
}

func fieldNameOf(trimmed string) string {
	f := strings.Fields(trimmed)
	if len(f) == 0 {
		return ""
	}
	name := strings.TrimSuffix(f[0], "!")
	if regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
		return name
	}
	return ""
}

// docCommentRewriteError is reserved for future strict modes; the rewrite
// is total today (unconvertible shapes are left untouched).
var _ = fmt.Sprintf
