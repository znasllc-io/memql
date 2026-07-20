package parser

import (
	"regexp"
	"strings"
)

// The #2618 codemods: collapse the long-form field ceremony into the
// sigil/enum-type/positional spellings. All three are line-based and
// conservative -- a line that does not match the expected field shape
// passes through untouched, commented occurrences are skipped, and
// the long forms keep parsing (the rewrites are corpus hygiene, not
// rejections). The dsl/ gates run these exact functions.

var (
	// fieldLineRe captures a field declaration's lead: indentation,
	// name, gap, type (with optional []-shorthand, parenthesized enum
	// values, and an existing sigil).
	fieldLineRe = regexp.MustCompile(`^([ \t]*)([A-Za-z_][A-Za-z0-9_]*)([ \t]+)((?:\[\])?[A-Za-z_][A-Za-z0-9_]*(?:\([^()]*\))?)(!?)`)
	requiredRe  = regexp.MustCompile(`[ \t]+@required\b`)
	enumAnnRe   = regexp.MustCompile(`[ \t]+@enum\(([^)]*)\)`)
	cacheTTLRe  = regexp.MustCompile(`@cache\([ \t]*ttl[ \t]*=[ \t]*"([0-9]+)"[ \t]*\)`)
	argsOpenRe  = regexp.MustCompile(`(^|[^A-Za-z0-9_])args[ \t]*\{`)
	toolOpenRe  = regexp.MustCompile(`^[ \t]*(?:tool|builtin|prompt)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
)

// RewriteRequiredSigil rewrites `name type @required ...` field lines
// to `name type! ...` (#2618a). Applies wherever the annotation is
// legal (args blocks and concept bodies share the field-line shape);
// a line whose @required sits after a // comment marker is prose and
// stays.
func RewriteRequiredSigil(src []byte) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	changed := false
	for i, line := range lines {
		if !requiredBeforeComment(line) {
			continue
		}
		m := fieldLineRe.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		out := requiredRe.ReplaceAllString(line, "")
		if line[m[10]:m[11]] != "!" { // group 5: no existing sigil
			typeEnd := m[9] // group 4 end: insert directly after the type
			out = out[:typeEnd] + "!" + out[typeEnd:]
		}
		lines[i] = out
		changed = true
	}
	if !changed {
		return src, nil
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// requiredBeforeComment reports whether the line carries a live
// @required annotation (not inside a // comment tail).
func requiredBeforeComment(line string) bool {
	loc := requiredRe.FindStringIndex(line)
	if loc == nil {
		return false
	}
	if c := strings.Index(line, "//"); c >= 0 && c < loc[0] {
		return false
	}
	return true
}

// RewriteEnumTypeArgs rewrites `name string(!) ... @enum("a","b") ...`
// fields to the first-class enum type `name enum("a","b")(!) ...`
// (#2618b). Scoped to args blocks AND tool/builtin/prompt bodies
// (their fields sit directly in the construct body): the concept
// builder never accepted
// the @enum annotation, so the double-statement only ever existed in
// those regions. Run after RewriteRequiredSigil so @required is
// already folded into the sigil.
func RewriteEnumTypeArgs(src []byte) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	depth := 0
	inArgs := false
	argsDepth := 0
	changed := false
	for i, line := range lines {
		code := line
		if c := strings.Index(code, "//"); c >= 0 {
			code = code[:c]
		}
		if inArgs && depth < argsDepth {
			inArgs = false
		}
		if !inArgs && (argsOpenRe.MatchString(code) || toolOpenRe.MatchString(code)) {
			inArgs = true
			argsDepth = depth + 1
			depth += strings.Count(code, "{") - strings.Count(code, "}")
			continue
		}
		if inArgs {
			if em := enumAnnRe.FindStringSubmatchIndex(code); em != nil {
				m := fieldLineRe.FindStringSubmatchIndex(line)
				if m != nil && line[m[8]:m[9]] == "string" {
					values := line[em[2]:em[3]]
					sigil := line[m[10]:m[11]]
					out := line[:m[8]] + "enum(" + values + ")" + sigil + line[m[11]:]
					out = enumAnnRe.ReplaceAllString(out, "")
					lines[i] = out
					changed = true
				}
			}
		}
		depth += strings.Count(code, "{") - strings.Count(code, "}")
	}
	if !changed {
		return src, nil
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// RewriteCachePositional rewrites `@cache(ttl="300")` to `@cache(300)`
// (#2618c) -- the registry's single ttl arg makes position unambiguous.
func RewriteCachePositional(src []byte) ([]byte, error) {
	out := cacheTTLRe.ReplaceAllString(string(src), "@cache($1)")
	if out == string(src) {
		return src, nil
	}
	return []byte(out), nil
}
