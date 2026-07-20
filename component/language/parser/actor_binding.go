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

// actorRefPattern matches a genuine actor-envelope read: `actor.`
// led by a non-word, non-dot character (or line start), so
// `event.actor.id` (the event envelope, a different object) and
// identifiers merely ending in "actor" never match.
var actorRefPattern = regexp.MustCompile(`(^|[^.\w])actor\.`)

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
	for i, h := range headers {
		start := h[0]
		end := len(text)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
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
		b.WriteString(ind + "@actor\n")
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
