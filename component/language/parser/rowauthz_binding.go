package parser

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The #2920 row-authorization declaration surface: who may see a
// concept's rows becomes a property of the CONCEPT, declared once, in
// place of the `actor.*` term an author must remember to type into
// every filter over it (#2803).
//
// PHASE 1 IS INERT BY CONSTRUCTION. This file parses, validates, and
// rewrites the declaration. Nothing reads the resulting tier at query
// time -- no predicate is injected, no filter compilation changes, and
// a construct that returned N rows before the annotation landed returns
// the same N rows after. The predicate column in the table below is
// documented so the vocabulary is DESIGNED against its eventual use,
// not so it gets applied now. `TestRowAuthzIsInert` (in the
// memory-nodes package) is the gate that keeps it that way.
//
// The detector and the codemod live in one file for the #2621 reason:
// the loader-side validator (component/database/memory-nodes) and the
// memqlmigrate rewrite must share ONE definition of what a declaration
// means, or they drift into disagreeing about the same annotation.
// The codemod never parses `@rowAuthz` itself -- it renders through
// FormatRowAuthz and reads through ParseRowAuthz, the same two
// functions the loader calls.

// RowAuthzAnnotation is the annotation name, spelled once.
const RowAuthzAnnotation = "rowAuthz"

// RowAuthzTier is the declared row-authorization tier of a concept.
// The four values are the buckets docs/public/operate/auth/
// per-row-authz-audit.md already defines in prose; this type is what
// makes an author DECLARE one instead of a regex inferring it per
// construct.
type RowAuthzTier string

const (
	// RowAuthzOwned -- rows belong to a user. Will eventually inject
	// `<Owner> == actor.userId`.
	RowAuthzOwned RowAuthzTier = "owned"
	// RowAuthzClusterOwner -- rows are administrative. Will
	// eventually inject `actor.isClusterOwner == true`.
	RowAuthzClusterOwner RowAuthzTier = "clusterOwner"
	// RowAuthzPublic -- rows are globally readable by intent. Injects
	// nothing, and must be spelled explicitly: "no annotation" and
	// "declared public" are different states, which is the whole
	// point of the tier existing (#2803).
	RowAuthzPublic RowAuthzTier = "public"
	// RowAuthzGranted -- visibility comes from a relationship. Will
	// eventually inject the named spec, gated on actor.userId.
	RowAuthzGranted RowAuthzTier = "granted"
)

// RowAuthzDecl is the parsed meaning of one `@rowAuthz(...)`
// annotation. Exactly one of Owner / Spec is set, and only for the
// tier that takes a parameter.
type RowAuthzDecl struct {
	Tier RowAuthzTier `json:"tier"`
	// Owner is the payload field compared against actor.userId.
	// Set only for RowAuthzOwned.
	Owner string `json:"owner,omitempty"`
	// Spec is the relationship spec that grants visibility.
	// Set only for RowAuthzGranted.
	Spec string `json:"spec,omitempty"`
}

// The declaration forms. The two parameterised tiers use the house
// keyword-arg spelling (`@relationship(field="x")`,
// `@displayCard(primary="name")`) rather than the `owner: field` form
// #2803's table sketched: `:` inside an annotation argument list lexes
// as TokenColon, while parseAttribute's named-arg branch tests for a
// TokenOperator, so the colon form does not parse and degrades to a
// flag before failing on `expected ')'`. Changing the shared lexer for
// one annotation buys nothing the `=` form does not already give.
//
//	@rowAuthz(public)
//	@rowAuthz(clusterOwner)
//	@rowAuthz(owner="ownerUserId")
//	@rowAuthz(via="spaceMember")
//
// The flag tiers and the keyword tiers occupy separate namespaces
// inside the argument list, which is what leaves room for the escape
// hatch #2920 requires the grammar to be ABLE to express without
// implementing: the pre-actor bootstrap path. `userByIdSystem`
// resolves `sub` -> user in order to BUILD the actor, so `actor.userId`
// is circular there -- component/auth/identity_resolver.go:79, and the
// query is @serverOnly for exactly that reason (#2800).
//
// NOT `userById`, which this comment named until memql#2984. That is a
// different query, gated by requiresOwnerOrAdmin, and a reader who
// followed the citation found a gated construct and concluded the
// constraint was imaginary. The constraint is real; only the name was
// wrong. A fifth tier
// is a new flag name or a new keyword; neither disturbs the four here.
const (
	rowAuthzArgOwner = "owner"
	rowAuthzArgVia   = "via"
)

// rowAuthzFlagTiers maps the bare-flag spellings to their tier.
var rowAuthzFlagTiers = map[string]RowAuthzTier{
	"public":       RowAuthzPublic,
	"clusterOwner": RowAuthzClusterOwner,
}

// rowAuthzKeywordTiers maps the keyword-arg spellings to their tier.
var rowAuthzKeywordTiers = map[string]RowAuthzTier{
	rowAuthzArgOwner: RowAuthzOwned,
	rowAuthzArgVia:   RowAuthzGranted,
}

// rowAuthzSpellings renders the accepted forms for a diagnostic, in a
// stable order.
func rowAuthzSpellings() string {
	return `@rowAuthz(public), @rowAuthz(clusterOwner), @rowAuthz(owner="<field>"), @rowAuthz(via="<spec>")`
}

// ParseRowAuthz is THE detector: it turns an `@rowAuthz(...)`
// attribute into its declared meaning, or into the diagnostic
// explaining why it has none. Both the loader and the codemod read
// declarations through this function and nothing else.
//
// The returned error never names the concept -- the caller holds that
// and wraps, the way applyConceptAttribute's siblings do.
func ParseRowAuthz(attr *Attribute) (*RowAuthzDecl, error) {
	if attr == nil {
		return nil, fmt.Errorf("@%s: nil attribute", RowAuthzAnnotation)
	}
	if attr.Name != RowAuthzAnnotation {
		return nil, fmt.Errorf("@%s: called on @%s", RowAuthzAnnotation, attr.Name)
	}

	// `@rowAuthz("public")` -- the single-string form. Rejected rather
	// than accepted as a synonym: one spelling per tier is what makes
	// the declaration greppable, and a second spelling is drift
	// surface with no upside.
	if attr.Value != nil {
		return nil, fmt.Errorf("@%s does not take a bare value -- write one of: %s",
			RowAuthzAnnotation, rowAuthzSpellings())
	}
	if len(attr.Args) == 0 {
		return nil, fmt.Errorf("@%s requires a tier -- write one of: %s",
			RowAuthzAnnotation, rowAuthzSpellings())
	}
	if len(attr.Args) > 1 {
		return nil, fmt.Errorf("@%s takes exactly one tier, got %d (%s) -- a concept declares a single floor",
			RowAuthzAnnotation, len(attr.Args), strings.Join(sortedArgNames(attr.Args), ", "))
	}

	// Exactly one entry; pull it out.
	var name string
	var raw any
	for k, v := range attr.Args {
		name, raw = k, v
	}

	if tier, ok := rowAuthzFlagTiers[name]; ok {
		// A flag tier carries no value: the parser stores a bare
		// identifier as `true`. `@rowAuthz(public="x")` is a
		// different shape and is not a spelling of this tier.
		if b, isBool := raw.(bool); !isBool || !b {
			return nil, fmt.Errorf("@%s(%s) takes no value -- write @%s(%s)",
				RowAuthzAnnotation, name, RowAuthzAnnotation, name)
		}
		return &RowAuthzDecl{Tier: tier}, nil
	}

	if tier, ok := rowAuthzKeywordTiers[name]; ok {
		s, isString := raw.(string)
		if !isString {
			return nil, fmt.Errorf("@%s(%s=...) requires a quoted %s -- write @%s(%s=%q)",
				RowAuthzAnnotation, name, rowAuthzValueNoun(name), RowAuthzAnnotation, name, rowAuthzValuePlaceholder(name))
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("@%s(%s=\"\") is empty -- name the %s the tier gates on",
				RowAuthzAnnotation, name, rowAuthzValueNoun(name))
		}
		decl := &RowAuthzDecl{Tier: tier}
		if tier == RowAuthzOwned {
			decl.Owner = s
		} else {
			decl.Spec = s
		}
		return decl, nil
	}

	return nil, fmt.Errorf("@%s: unknown tier %q -- write one of: %s",
		RowAuthzAnnotation, name, rowAuthzSpellings())
}

func rowAuthzValueNoun(arg string) string {
	if arg == rowAuthzArgOwner {
		return "field name"
	}
	return "spec name"
}

func rowAuthzValuePlaceholder(arg string) string {
	if arg == rowAuthzArgOwner {
		return "ownerUserId"
	}
	return "spaceMember"
}

// sortedArgNames renders an attribute's argument names in a stable
// order for a diagnostic. (The package's existing sortedKeys takes a
// membership set, not an arg map.)
func sortedArgNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FormatRowAuthz renders a declaration in its canonical spelling. The
// codemod emits through this and only this, so anything it can write
// is by construction something ParseRowAuthz reads back to the same
// decl -- the round-trip the loader/codemod agreement test asserts.
func FormatRowAuthz(d RowAuthzDecl) (string, error) {
	switch d.Tier {
	case RowAuthzPublic, RowAuthzClusterOwner:
		return fmt.Sprintf("@%s(%s)", RowAuthzAnnotation, d.Tier), nil
	case RowAuthzOwned:
		if strings.TrimSpace(d.Owner) == "" {
			return "", fmt.Errorf("@%s: tier %q needs an owner field", RowAuthzAnnotation, d.Tier)
		}
		return fmt.Sprintf("@%s(%s=%q)", RowAuthzAnnotation, rowAuthzArgOwner, d.Owner), nil
	case RowAuthzGranted:
		if strings.TrimSpace(d.Spec) == "" {
			return "", fmt.Errorf("@%s: tier %q needs a spec name", RowAuthzAnnotation, d.Tier)
		}
		return fmt.Sprintf("@%s(%s=%q)", RowAuthzAnnotation, rowAuthzArgVia, d.Spec), nil
	default:
		return "", fmt.Errorf("@%s: unknown tier %q", RowAuthzAnnotation, d.Tier)
	}
}

// rowAuthzDeclPattern matches an `@rowAuthz` annotation.
//
// NOT line-anchored, unlike the sibling actor-binding detector. The
// parser accepts several annotations on one line, so
// `@description("d") @rowAuthz(public)` is a real declaration that a
// `^[ \t]*@rowAuthz` pattern does not see -- and not seeing it made
// the codemod insert a SECOND declaration above a concept that already
// had one, silently replacing a hand-authored tier. False positives
// from prose are handled by blanking rather than by anchoring, which
// is the stronger guard: `@` cannot appear inside an identifier, so
// there is nothing else for `@rowAuthz` to be.
var rowAuthzDeclPattern = regexp.MustCompile(`@` + RowAuthzAnnotation + `\b`)

// RowAuthzDeclaredInSource reports whether the source carries an
// @rowAuthz annotation. Used for the codemod's idempotency check -- a
// concept that already declares a tier is never rewritten, so a re-run
// is a no-op and a hand-authored declaration is never clobbered.
//
// Scanned on the comment- and string-blanked view so a doc comment
// that MENTIONS the annotation does not read as a declaration.
func RowAuthzDeclaredInSource(source string) bool {
	return rowAuthzDeclPattern.MatchString(blankCommentsAndStrings(source))
}

// conceptHeaderRe matches a concept declaration line. Group 1 is the
// indentation, group 2 the declared name.
var conceptHeaderRe = regexp.MustCompile(`(?m)^([ \t]*)concept[ \t]+([\p{L}_][` + identChar + `]*)`)

// ConceptHeader is one concept declaration located in a source file.
type ConceptHeader struct {
	Name string // the declared name, e.g. "plan"
	// Start is the byte offset of the header line's first character.
	Start int
	// End is the exclusive byte offset of the construct's closing
	// brace.
	End int
	// PreambleStart is the byte offset where the contiguous run of
	// annotation and comment lines above the header begins.
	PreambleStart int
	// Indent is the header's leading whitespace.
	Indent string
}

// ConceptHeaders locates every concept declaration in src.
//
// Headers are matched on the COMMENT- AND STRING-BLANKED view, not on
// raw source, so the word "concept" inside a doc comment or a
// @description string can never be mistaken for a declaration. The
// blanker is length-preserving, so every offset it yields indexes the
// raw text unchanged -- which is what keeps this off the class of bug
// where a rewriter locates on one view and slices on another (#2948).
func ConceptHeaders(src string) []ConceptHeader {
	blanked := blankCommentsAndStrings(src)
	matches := conceptHeaderRe.FindAllStringSubmatchIndex(blanked, -1)
	out := make([]ConceptHeader, 0, len(matches))
	for _, m := range matches {
		start := m[0]
		out = append(out, ConceptHeader{
			Name:          blanked[m[4]:m[5]],
			Start:         start,
			End:           conceptEnd(blanked, start),
			PreambleStart: preambleStart(src, start),
			Indent:        blanked[m[2]:m[3]],
		})
	}
	return out
}

// conceptEnd returns the exclusive end offset of the concept whose
// header starts at headerStart: the matching close of its body brace.
// Operates on the already-blanked view, so brace counting cannot be
// thrown by a brace inside a string or a comment.
func conceptEnd(blanked string, headerStart int) int {
	braceIdx := strings.IndexByte(blanked[headerStart:], '{')
	if braceIdx < 0 {
		return len(blanked)
	}
	depth := 0
	for i := headerStart + braceIdx; i < len(blanked); i++ {
		switch blanked[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(blanked)
}

// RewriteRowAuthz inserts a `@rowAuthz(...)` line above every concept
// named in tiers whose preamble does not already declare one. It is
// the codemod half of #2920, and it is re-runnable: a concept that
// already carries a declaration -- inferred on an earlier run or
// written by hand -- is left exactly as it was.
//
// Concepts absent from tiers are untouched, which is how "the
// inference had no evidence for this one" stays visible as an
// undeclared concept rather than being papered over with a guess.
func RewriteRowAuthz(src []byte, tiers map[string]RowAuthzDecl) ([]byte, error) {
	if len(tiers) == 0 {
		return src, nil
	}
	text := string(src)
	headers := ConceptHeaders(text)
	if len(headers) == 0 {
		return src, nil
	}
	var b strings.Builder
	prev := 0
	changed := false
	// prevEnd is where the previous concept's body closed. Everything
	// from there to this concept's header belongs to THIS concept --
	// annotations, comments, and blank lines between them.
	prevEnd := 0
	for _, h := range headers {
		// Header STARTS are monotonic (the regex scans forward), but
		// header ENDS are not: a spurious header -- a field named
		// `concept`, or a real one in a file whose braces do not
		// balance -- closes on the first `{...}` after it, which can
		// be before the enclosing construct's close. So `prevEnd` can
		// overshoot this header, and `text[prevEnd:h.End]` would slice
		// backwards and panic on input this function is supposed to
		// merely decline to rewrite.
		//
		// Clamping to the preamble bound restores the invariant
		// (PreambleStart <= Start <= End) and does the right thing for
		// the silent-corruption variant too: when a spurious header
		// pushes prevEnd past a REAL following concept, that concept's
		// region would otherwise be empty, its existing declaration
		// invisible, and a duplicate inserted.
		regionStart := prevEnd
		if regionStart > h.Start {
			regionStart = h.PreambleStart
		}
		regionEnd := h.End
		if regionEnd < h.Start {
			regionEnd = h.Start
		}
		region := text[regionStart:regionEnd]
		if h.End > prevEnd {
			prevEnd = h.End
		}

		decl, want := tiers[h.Name]
		if !want {
			continue
		}
		// A declaration anywhere in that region means this concept is
		// already spoken for.
		//
		// The region runs from the previous concept's closing brace,
		// NOT from h.PreambleStart. preambleStart stops at the first
		// blank line, and the parser happily attaches an annotation
		// separated from its header by one:
		//
		//	@rowAuthz(public)
		//
		//	concept thing { ... }
		//
		// loads as a real `public` declaration. Bounding at the
		// preamble made that invisible, so the codemod inserted a
		// SECOND declaration -- and since a duplicate is a load error,
		// a re-run turned a loadable tree into one that refuses to
		// boot. Everything between two concepts is whitespace,
		// comments or annotations belonging to the following one, so
		// the wider region cannot pick up a neighbour's declaration.
		if RowAuthzDeclaredInSource(region) {
			continue
		}
		line, err := FormatRowAuthz(decl)
		if err != nil {
			return nil, fmt.Errorf("concept %s: %w", h.Name, err)
		}
		b.WriteString(text[prev:h.Start])
		b.WriteString(h.Indent)
		b.WriteString(line)
		b.WriteString("\n")
		prev = h.Start
		changed = true
	}
	if !changed {
		return src, nil
	}
	b.WriteString(text[prev:])
	return []byte(b.String()), nil
}

// BlankCommentsAndStrings is the exported form of the blanker, for the
// memqlmigrate codemod, which has to scan construct bodies in the same
// coordinate space this package's header location uses.
func BlankCommentsAndStrings(source string) string {
	return blankCommentsAndStrings(source)
}

// blankCommentsAndStrings replaces the contents of line comments,
// block comments and string literals with spaces, preserving every
// byte offset and every newline so offsets into the result index the
// original unchanged.
//
// It is a strict superset of actor_binding.go's stripCommentsAndStrings,
// which predates block-comment support in the lexer and therefore does
// not blank `/* ... */`. This one does, and it consumes backslash
// escapes inside strings so `"he said \"concept\""` cannot end the
// string early. Kept local rather than widening the actor-binding
// helper: that one backs a shipped load rule whose behaviour is
// asserted by its own tests, and changing what it considers a comment
// is a separate change from adding this annotation.
func blankCommentsAndStrings(source string) string {
	var b strings.Builder
	b.Grow(len(source))
	const (
		code = iota
		inLineComment
		inBlockComment
		inStr
	)
	state := code
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch state {
		case inLineComment:
			if c == '\n' {
				state = code
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inBlockComment:
			if c == '*' && i+1 < len(source) && source[i+1] == '/' {
				b.WriteString("  ")
				i++
				state = code
				continue
			}
			if c == '\n' {
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inStr:
			switch {
			case c == '\\' && i+1 < len(source) && source[i+1] != '\n':
				// Blank the escape and the byte it escapes together,
				// so `\"` cannot be read as a closing quote.
				b.WriteString("  ")
				i++
			case c == '"':
				state = code
				b.WriteByte(c)
			case c == '\n':
				// An unterminated string ends at the newline, the
				// same recovery the lexer performs.
				state = code
				b.WriteByte(c)
			default:
				b.WriteByte(' ')
			}
		default:
			switch {
			case c == '"':
				state = inStr
				b.WriteByte(c)
			case c == '/' && i+1 < len(source) && source[i+1] == '/':
				state = inLineComment
				b.WriteByte(' ')
			case c == '/' && i+1 < len(source) && source[i+1] == '*':
				b.WriteString("  ")
				i++
				state = inBlockComment
			default:
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
