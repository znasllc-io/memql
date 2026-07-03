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

// nonProceduralHeaders lists top-level construct keywords the generic
// parser cannot ingest natively and must therefore step over. It is
// EMPTY today: every author-facing construct keyword now has a native
// decl parser registered in parser.go's topLevelDeclParsers dispatch
// table (concept / shape / provider / builtin / tool / prompt / policy
// / spec / trait / seed / action / capability), so ParseFileSource can
// parse a consolidated file end-to-end without any block being removed.
//
// History: the strip step existed while shape / builtin / prompt /
// seed / spec / trait lacked generic-parser support and had only their
// dedicated runtime loaders (shape_loader.go, spec_loader.go, ...).
// Stripping their bodies let the generic parser skip past them so a
// file mixing procedural + non-procedural constructs still parsed. That
// also blinded the memqllint / dslimports lint gate to malformed
// bodies in those kinds -- a garbage-body spec / shape produced ZERO
// diagnostics because the strip removed the broken source before the
// parser ever saw it (memql#2356 / epic #2351). provider / tool /
// policy were removed from this list earlier (sub-epic #309 children
// #316 / #317, sub-epic #329 child #333) once their native parsers
// landed; #2356 removes the final six now that the same is true of
// them, so the lint gate parses every kind through its real parser and
// surfaces broken bodies as diagnostics.
//
// The slice is retained (empty) so the strip machinery degrades to a
// guarded no-op rather than being deleted outright: if a future
// construct kind ever needs to be stepped over before it has a native
// parser, adding its keyword here re-arms the strip in one line.
var nonProceduralHeaders = []string{}

// nonProceduralRe matches `<keyword> [<Concept>] NAME {` at column 0
// (including leading whitespace). Two header shapes are supported:
//
//   - bare form, used by provider / builtin / prompt / tool / policy
//     / spec / trait and by concept-agnostic shape declarations:
//
//     shape spaceCard {
//     provider chat54Mini {
//     spec isHumanParticipant {
//     trait isActiveRecord {
//
//   - concept-bound form, used by shape + seed declarations that
//     bind to a concept in their signature (mirrors the
//     `query <Concept> <name>` / `mutation <Concept> <name>` patterns):
//
//     shape workspace workspaceFull {
//     shape invocation workerInvocationFull {
//     seed agent assistant {
//     seed skill workbench-baseline {
//
// The optional `(?:[A-Za-z_][A-Za-z0-9_-]*[ \t]+)?` group consumes
// the concept binding when present; the trailing identifier captures
// the construct's own name in either form. Names accept hyphens (the
// seed skill catalog uses kebab-case slugs like `workbench-baseline`),
// concept-binding identifiers don't need them in practice but share
// the same class for simplicity.
//
// Without the optional group AND the seed/spec/trait keywords,
// concept-bound shapes / seed-with-concept-binding / spec / trait
// declarations survive the strip and the generic parser chokes on
// the keyword as an unrecognized top-level token -- a class of
// failure dslimports.Load was silently swallowing pre-#293. See the
// fallback in dslimports.go for the matching swallow-narrowing.
//
// When nonProceduralHeaders is empty the alternation would collapse to
// `()` and match every line, so the regex is left nil and the strip
// functions short-circuit to no-ops. Re-populating the header list
// re-arms it automatically.
var nonProceduralRe = func() *regexp.Regexp {
	if len(nonProceduralHeaders) == 0 {
		return nil
	}
	return regexp.MustCompile(
		`(?m)^[ \t]*(` +
			strings.Join(nonProceduralHeaders, "|") +
			`)[ \t]+(?:[A-Za-z_][A-Za-z0-9_-]*[ \t]+)?([A-Za-z_][A-Za-z0-9_-]*)[ \t]*\{`,
	)
}()

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
	if nonProceduralRe == nil {
		return source
	}
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
	if nonProceduralRe == nil {
		return false
	}
	return nonProceduralRe.MatchString(source)
}
