package parser

import (
	"regexp"
	"strings"
)

// The #2621 actor-binding surface: reading the auth envelope is a
// declared capability. `actor.*` in a query/mutate/logic/automation
// body without `@actor` in the preamble is a load error (modeled on
// the logic event-binding rule, memql#1706: used-but-undeclared
// errors, declared-but-unused is fine). The detector and the codemod
// live here so the loader-side validator (component/memql) and the
// memqlmigrate rewrite share ONE definition of "reads the actor".

// identChar is the character class the LEXER accepts inside an identifier,
// spelled for regexp. It mirrors isIdentifierCharNoColon (lexer.go):
// unicode.IsLetter, unicode.IsDigit, `_` and `-`. Go's `\w` is ASCII-only, so
// patterns written with it are narrower than the grammar they police -- which
// is the whole of memql#2809.
//
// `\p{Nd}`, not `\p{N}`: unicode.IsDigit is Nd only, while `\p{N}` also admits
// Nl/No (`Ⅰ`, `½`, `②`). Those are lexer errors, so the wider class could only
// ever disagree on input that cannot load -- but the point of this const is to
// BE the lexer's class, and an approximation is how the original drifted.
//
// `-` is included even though it reads like an operator. The lexer joins it
// UNCONDITIONALLY (the "only when the next rune is alphanumeric" clause in
// that function's comment governs `:` and `.`, not `-`), so `my-actor.userId`
// and `actor.userId-1` are each ONE identifier token. Excluding it left both
// directions of this bug alive:
//
//	my-actor.userId   guard saw `-` as a boundary  -> read as an actor ref,
//	                  so a valid file load-rejected for a missing @actor
//	actor.userId-1    member truncated to `userId` -> a member that is NOT in
//	                  the closed set passed the validator silently
//
// Arithmetic is unaffected: subtraction needs surrounding spaces
// (`actor.userId - 1` lexes as three tokens), which these patterns already
// stop at.
const identChar = `\p{L}\p{Nd}_\-`

// actorRefPattern matches a genuine actor-envelope read: `actor.`
// led by a non-identifier, non-dot character (or line start), so
// `event.actor.id` (the event envelope, a different object) and
// identifiers merely ending in "actor" never match.
//
// The leading class must use identChar rather than `\w`: with the ASCII-only
// class a non-ASCII letter immediately before `actor.` counted as a boundary,
// so `mïactor.userId` -- one ordinary identifier -- read as an actor
// reference. Under the #2621 used-but-undeclared rule that is a LOAD ERROR on
// valid DSL, not merely a bad message.
var actorRefPattern = regexp.MustCompile(`(^|[^.` + identChar + `])actor\.`)

// actorDeclPattern matches an `@actor` annotation line (bare or with
// arguments -- the seed-file `@actor("system")` is a different
// construct on a different keyword and never reaches this check).
var actorDeclPattern = regexp.MustCompile(`(?m)^[ \t]*@actor\b`)

// ActorRefInSource reports whether the comment- and string-stripped
// source reads the actor envelope.
func ActorRefInSource(source string) bool {
	return actorRefPattern.MatchString(stripCommentsAndStrings(source))
}

// ActorDeclaredInSource reports whether the source carries an @actor
// annotation line.
func ActorDeclaredInSource(source string) bool {
	return actorDeclPattern.MatchString(source)
}

// stripCommentsAndStrings blanks // line-comment tails and string
// literal contents (newline-terminated, no escape processing --
// matching the sibling scanners), so prose like "actor.rank" in a
// comment or a @description string never counts as a read.
func stripCommentsAndStrings(source string) string {
	var b strings.Builder
	b.Grow(len(source))
	inString := false
	inComment := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inString:
			if c == '"' || c == '\n' {
				inString = false
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(source) && source[i+1] == '/':
			inComment = true
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// actorConstructHeaderRe matches the declaration line of the four
// actor-capable construct kinds. Group 1 is the indentation.
var actorConstructHeaderRe = regexp.MustCompile(`(?m)^([ \t]*)(?:query|mutate|logic|automation)[ \t]+[A-Za-z_]`)

// RewriteActorBinding inserts a bare `@actor` line above every
// query/mutate/logic/automation whose body reads the actor envelope
// and whose preamble does not already declare it (#2621). Other
// construct kinds (specs and traits load-reject direct actor reads;
// shapes use @actor as a kind marker; seeds' @actor("system") is a
// different construct) are untouched by construction -- the header
// regex only matches the four function kinds.
func RewriteActorBinding(src []byte) ([]byte, error) {
	text := string(src)
	headers := actorConstructHeaderRe.FindAllStringSubmatchIndex(text, -1)
	if len(headers) == 0 {
		return src, nil
	}
	var b strings.Builder
	prev := 0
	changed := false
	for _, h := range headers {
		start := h[0]
		end := constructEnd(text, start)
		construct := text[start:end]
		// The preamble (annotations above the header) belongs to this
		// construct; scan back over contiguous annotation/comment lines.
		pStart := preambleStart(text, start)
		region := text[pStart:end]

		if !ActorRefInSource(construct) || ActorDeclaredInSource(region) {
			continue
		}
		ind := text[h[2]:h[3]]
		b.WriteString(text[prev:start])
		b.WriteString(ind)
		b.WriteString("@actor\n")
		prev = start
		changed = true
	}
	if !changed {
		return src, nil
	}
	b.WriteString(text[prev:])
	return []byte(b.String()), nil
}

// preambleStart walks back from a construct header over the
// contiguous run of annotation and comment lines that belong to it.
func preambleStart(text string, headerStart int) int {
	lineStart := headerStart
	for lineStart > 0 {
		prevEnd := lineStart - 1 // the \n before this line
		ps := strings.LastIndexByte(text[:prevEnd], '\n') + 1
		trimmed := strings.TrimSpace(text[ps:prevEnd])
		if trimmed == "" || (!strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "//")) {
			break
		}
		lineStart = ps
	}
	return lineStart
}

// constructEnd returns the exclusive end offset of the construct whose
// header starts at headerStart: the matching close of its body brace,
// or end-of-line for the braceless terse arrow form. Bounding at the
// real body close keeps a following construct of another kind (a spec
// or shape declared after a query in the same file) out of the
// region -- next-header bounding leaked those bodies in (#2622).
func constructEnd(text string, headerStart int) int {
	lineEnd := strings.IndexByte(text[headerStart:], '\n')
	if lineEnd < 0 {
		lineEnd = len(text) - headerStart
	}
	braceIdx := strings.IndexByte(text[headerStart:headerStart+lineEnd], '{')
	if braceIdx < 0 {
		return headerStart + lineEnd // terse arrow form: one line, no body
	}
	depth := 0
	inString := false
	inComment := false
	for i := headerStart + braceIdx; i < len(text); i++ {
		c := text[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
			}
		case inString:
			if c == '"' || c == '\n' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(text) && text[i+1] == '/':
			inComment = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(text)
}

// ActorUndeclaredRefOffsets returns the byte offset (into src) of every
// actor-envelope read inside a query/mutate/logic/automation construct
// whose preamble lacks @actor -- the edit-time mirror (#2622) of the
// load rule, sharing the same header, preamble, and detection
// internals so the engine error and the editor squiggle cannot drift.
func ActorUndeclaredRefOffsets(src string) []int {
	headers := actorConstructHeaderRe.FindAllStringSubmatchIndex(src, -1)
	var out []int
	for _, h := range headers {
		start := h[0]
		end := constructEnd(src, start)
		construct := src[start:end]
		region := src[preambleStart(src, start):end]
		if ActorDeclaredInSource(region) {
			continue
		}
		stripped := stripCommentsAndStrings(construct)
		for _, m := range actorRefPattern.FindAllStringSubmatchIndex(stripped, -1) {
			out = append(out, start+m[3]) // group 1 end == the `actor` token start
		}
	}
	return out
}

// actorMemberPattern captures the member name of an actor-envelope
// read: the same anchored `actor.` shape as actorRefPattern, plus the
// dotted member. Group 2 is the member; the leading class keeps
// `event.actor.id` (the event envelope stamp) out.
//
// The member class matches what the lexer admits, not ASCII. An ASCII-only
// class stopped at the first non-ASCII byte, so `actor.naïveProp` reported the
// member as `actor.na` and underlined two characters. The VERDICT was still
// right -- the envelope is a closed set of ASCII names, so any member
// containing a non-ASCII letter is invalid either way -- but the message named
// a token absent from the author's file, which reads as the tool being
// confused rather than as a real finding.
var actorMemberPattern = regexp.MustCompile(`(^|[^.` + identChar + `])actor\.([\p{L}_][` + identChar + `]*)`)

// ActorMemberRef is one actor member read in a source file: the member
// name and its byte offset (pointing at the member token, so editors
// can underline exactly the wrong word).
type ActorMemberRef struct {
	Name   string
	Offset int
}

// ActorMemberRefs returns every actor-envelope member read in the
// comment- and string-stripped source. Shared by the load-time
// validator and the sense diagnostic (#2625) so the two cannot drift
// in what they consider a read.
func ActorMemberRefs(source string) []ActorMemberRef {
	stripped := stripCommentsAndStrings(source)
	var out []ActorMemberRef
	for _, m := range actorMemberPattern.FindAllStringSubmatchIndex(stripped, -1) {
		out = append(out, ActorMemberRef{
			Name:   stripped[m[4]:m[5]],
			Offset: m[4],
		})
	}
	return out
}
