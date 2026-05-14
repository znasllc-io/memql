package parser

import (
	"regexp"
	"strings"
)

// strip_non_procedural.go removes struct-form declarations that the
// generic parser doesn't natively understand (shape, provider,
// builtin, prompt, tool, policy, automation) from a source so the
// remaining content (imports + concepts + queries + mutations +
// specs + automations + logic + procedural funcs) can be parsed
// cleanly.
//
// Why this exists: the new consolidated tree puts every construct
// kind in one file per entity. The parser pipeline has dedicated
// rewriters for spec / trait / query / mutation / logic /
// automation / file-top-args, but shape / provider / builtin /
// prompt / tool keep their own loaders + parsers (shape_parser.go,
// provider_loader.go, ...) that read these blocks directly.
//
// For the generic parser to ingest a consolidated file, the blocks
// it can't parse have to be removed. This file does that with a
// regex-driven walker that finds each non-procedural construct
// header + matching brace, replaces the block with a comment
// noting the original construct name.
//
// The stripped construct kinds still get loaded -- by their
// dedicated loaders walking the same FS in parallel.

// nonProceduralHeaders lists every top-level construct keyword the
// generic parser doesn't understand. Adding more is a one-line
// extension; the stripping logic is keyword-agnostic.
var nonProceduralHeaders = []string{
	"shape", "provider", "builtin", "prompt", "tool", "policy",
}

// nonProceduralRe matches `<keyword> NAME {` at column 0, including
// leading whitespace. The trailing `{` marks the start of the body
// that brace-matching closes out.
var nonProceduralRe = regexp.MustCompile(
	`(?m)^[ \t]*(` +
		strings.Join(nonProceduralHeaders, "|") +
		`)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
)

// StripNonProceduralBlocks finds every struct-form construct
// declaration that's not procedural (`func (X) name`) and removes
// the block from the source, replacing it with a single-line
// comment of the form `// <stripped: kind name>`. The comment is
// placed at the original block header position so downstream
// error messages with line numbers still point at a reasonable
// location.
//
// Preamble attribute lines (@-prefixed) immediately above the
// stripped block are also removed -- they're decorators on the
// stripped declaration. This avoids leaving orphan @-attributes
// hanging over the next surviving declaration.
//
// Pass-through if the source contains none of the targeted
// constructs.
func StripNonProceduralBlocks(source string) string {
	matches := nonProceduralRe.FindAllStringSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return source
	}

	// Process in reverse so byte offsets stay stable.
	out := source
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		headerStart := m[0]
		headerEnd := m[1]
		kindStart := m[2]
		kindEnd := m[3]
		nameStart := m[4]
		nameEnd := m[5]
		kind := out[kindStart:kindEnd]
		name := out[nameStart:nameEnd]

		// Find the matching close brace.
		openIdx := headerEnd - 1
		closeIdx := findMatchingCloseBrace(out, openIdx)
		if closeIdx < 0 {
			// Malformed; bail out without modifying this block.
			continue
		}

		// Walk backwards from headerStart to pick up the @-attribute
		// preamble. Stop at a blank line, a comment line that isn't
		// part of an attribute run, or another declaration.
		preambleStart := headerStart
		for k := headerStart - 1; k >= 0; k-- {
			lineStart := strings.LastIndexByte(out[:k], '\n') + 1
			line := strings.TrimRight(out[lineStart:k+1], "\r\n")
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "@") {
				preambleStart = lineStart
				k = lineStart - 1 // continue from the line above
				continue
			}
			break
		}

		replacement := "// <stripped: " + kind + " " + name + ">\n"
		out = out[:preambleStart] + replacement + out[closeIdx+1:]
	}

	return out
}

// LooksLikeNonProcedural reports whether the source contains any
// stripable non-procedural struct-form construct.
func LooksLikeNonProcedural(source string) bool {
	return nonProceduralRe.MatchString(source)
}
