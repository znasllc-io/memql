package dslgate

// clause.go is the clause-reading machinery the contract gates run on: strip
// comments, find a construct's body, pull its filter clause out, and decide
// what that clause's boolean STRUCTURE guarantees.
//
// Every function here was written in test/dslconformance and is moved
// VERBATIM (comments included -- each paragraph records a defect that reached
// the corpus). memql#3629 moves the gates themselves to load time, and a gate
// cannot move without the machinery it reads with. The test package now
// delegates to these instead of keeping a second copy: the recurring failure
// in this area is two detectors that drift, and the drift is always
// fail-OPEN in the security direction.

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslclause"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// ConstructHeaderRe matches the header line of a row-accessing construct.
//
// The keyword set is `query|mutate|seed` and NOT `mutation`: memql#3013
// renamed the keyword `mutation` -> `mutate`, so a classifier looking for
// `mutation` walked queries only and every mutation and seed in the tree was
// invisible to it. component/language/dslspec already hard-fails if `mutation`
// is still a construct keyword; the classifier never got the memo.
var ConstructHeaderRe = regexp.MustCompile(`(?m)^[ \t]*(query|mutate|seed)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// UserScopeFieldRe matches a user-scope payload field referenced BARE.
//
// The detector this replaces looked for the `payload.`-prefixed spelling
// (`payload.ownerUserId`, ...), which epic #2292 retired -- payload properties
// are referenced bare in filters now. So it was searching for a spelling the
// corpus had migrated off, reported 0 flagged across ~198 constructs, and that
// zero read as "audited and clean" rather than "not measuring what its name
// says" (memql#2799).
//
// The leading `(^|[^.\w])` group is what keeps `actor.userId`, `args.userId`
// and `row.createdBy` out: a dotted reference is a caller/envelope/intrinsic
// read, not the row's own user-scope column. Go's RE2 has no lookbehind, so the
// boundary is a consumed group rather than `(?<![.\w])`.
var UserScopeFieldRe = regexp.MustCompile(`(^|[^.\w])(ownerUserId|userId|actorUserId|targetId|createdBy|requestedBy)\b`)

// AdminGateRe matches a predicate that establishes admin-ness WHEN TRUE.
//
// POLARITY is load-bearing, and a bare strings.Contains does not carry it: the
// composition rule asks whether a FALSE gate zeroes the row set, so the leaf
// must be a term that is false for a non-admin. `actor.isClusterOwner!=true`
// and `==false` contain the same identifier and invert the meaning -- under
// them a non-owner who satisfies the other conjunct gets rows and the cluster
// owner gets none, which is the very failure this gate exists to refuse.
// StripLeadingNot deliberately leaves `!=` alone (it is a comparison, not a
// negation), so nothing upstream catches it either.
//
// Word boundaries matter for the same reason: `requiresClusterOwnerXyz` is a
// different identifier, and a substring test accepted it as the gate.
//
// The spec alternatives are ordered longest-first so `requiresOwnerOrAdmin`
// cannot be consumed as `requiresOwner`.
//
// Which of these names is DECLARED, and which the corpus actually uses, is not
// written here (memql#3016): both statements went stale once, and a stale claim
// in the definition of the vocabulary is how the classifier drifted from it in
// the first place. Both facts are COMPUTED, by two tests in
// test/dslconformance/admin_gate_test.go that read the tree instead of
// describing it -- TestAdminGateNamesAreDeclaredOrRecorded (every name here is
// declared or recorded) and TestEveryDeclaredActorGateIsRecognised (every
// declared caller-scope spec appears here).
//
// The converse direction is not symmetry for its own sake. A gate this pattern
// does not know is not a gate: AdminGateLeaf returns false for it, the
// composition rule never runs on a filter that uses it, and the classifier does
// not count it as caller-scoped. It found `requiresDeveloperOrAbove`
// (dsl/deployment/specs.memql) plus `forgeDeveloper` and `forgeApprover`
// (dsl/forge/specs.memql), the latter two used as LIVE filter conjuncts -- two
// authorization gates in production filters the composition rule had never once
// run on. They were correctly written, which was luck rather than a checked
// property.
var AdminGateRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner[ \t]*==[ \t]*true|requiresDeveloperOrAbove|requiresOwnerOrAdmin|requiresClusterOwner|requiresAdmin|requiresOwner|forgeApprover|forgeDeveloper)([^A-Za-z0-9_]|$)`)

// AdminGateMentionRe is the POLARITY-BLIND twin, and the two must stay
// separate: selection has to be broad and assertion strict.
//
// If the gate selected constructs with the strict predicate, an inverted
// filter (`actor.isClusterOwner!=true`) would simply not be recognised as
// admin-gated, get skipped, and sail through -- swapping one fail-open for
// another. Selecting on any MENTION and then demanding the strict form is what
// turns the inverted spelling into an error instead of a silence.
var AdminGateMentionRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner|requiresDeveloperOrAbove|requiresOwnerOrAdmin|requiresClusterOwner|requiresAdmin|requiresOwner|forgeApprover|forgeDeveloper)([^A-Za-z0-9_]|$)`)

// OwnerScopeLeaf and AdminGateLeaf name the leaf predicates the authz gates
// recognise. They live together so the classification gate and the
// composition gate cannot drift about what counts as a caller check -- the
// drift these gates keep being filed for.
func OwnerScopeLeaf(pred string) bool { return strings.Contains(pred, "actor.userId") }

// AdminGateLeaf reports whether a single predicate establishes admin-ness.
func AdminGateLeaf(pred string) bool { return AdminGateRe.MatchString(pred) }

// MentionsAdminGate reports whether a clause NAMES an admin gate, in any
// polarity. Selection, not assertion -- see AdminGateMentionRe.
func MentionsAdminGate(clause string) bool { return AdminGateMentionRe.MatchString(clause) }

// CallerScopeLeaf reports whether a single predicate ties the row to THIS
// CALLER at all -- either because the caller owns it, or because the caller
// administers the cluster.
//
// It exists for the COMPOSITE row-authz tier (memql#4312), whose injected
// predicate is `(<owner>==actor.userId)||(actor.isClusterOwner==true)`: "the
// owner, or a cluster owner". Neither OwnerScopeLeaf nor AdminGateLeaf holds
// on every arm of that disjunction, and ClauseGuarantees is right to say so
// for each of them separately -- but the DISJUNCTION of the two is exactly
// the floor the tier declares, and `ClauseGuarantees(clause, CallerScopeLeaf)`
// is the question that has the right answer.
//
// WHY THIS DOES NOT REOPEN THE memql#2839 FAIL-OPEN. That gate refuses an
// admin gate ORed with a SELECTION term:
//
//	fromE164==args.e164 || actor.isClusterOwner==true   // fail-open
//	ownerUserId==actor.userId || actor.isClusterOwner==true   // the floor
//
// On the first, a false admin gate zeroes nothing -- any caller who supplies
// `fromE164` still reads rows, which is the bypass. On the second, a false
// admin gate leaves exactly the caller's own rows. The difference is entirely
// in the OTHER arm, and this predicate is what reads it: `fromE164==args.e164`
// satisfies neither half, so the fail-open shape is still refused. Composing
// the two leaves rather than loosening either one is what keeps that true.
func CallerScopeLeaf(pred string) bool { return OwnerScopeLeaf(pred) || AdminGateLeaf(pred) }

// BlankComments removes BOTH comment forms -- `//` line and `/* */` block --
// via the parser's own offset-preserving blanker, so a construct's structure
// is read the way the lexer reads it.
//
// Three hand-rolled attempts preceded this, each blind to something the
// previous one handled (memql#2840 review rounds 1-3): the first ended an
// update block at a nested `}`, the second truncated a filter clause at the
// `//` inside a URL, the third handled `//` but not `/* */`, so a gate term
// commented out with `/* && requiresOwnerOrAdmin */` still read as live.
func BlankComments(s string) string { return languageParser.BlankComments(s) }

// StructureOf returns line with double-quoted string contents blanked, so
// brace counting sees only structural punctuation. Comments must already be
// blanked by BlankComments -- BlankComments deliberately preserves string
// CONTENT, which is what a `}` inside a string literal hides behind.
func StructureOf(line string) string {
	out := make([]byte, 0, len(line))
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inString && c == '\\' && i+1 < len(line):
			out = append(out, ' ', ' ')
			i++
		case c == '"':
			inString = !inString
			out = append(out, ' ')
		case inString:
			out = append(out, ' ')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// MatchingClose walks src from openIdx (position of `{`) and returns
// the index of the matching `}`. String + line-comment aware.
func MatchingClose(src string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(src) || src[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if inString {
			if c == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				nl := strings.IndexByte(src[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// RowSelectionSurface returns only the parts of a body that SELECT rows: the
// `filter` clause, and an `update { }` block's `id:` line.
//
// Scope matters as much as spelling. Matching the WHOLE body flags every
// `create*` mutation that stamps an owner on insert (`ownerUserId:
// args.ownerUserId`) -- 43 constructs tree-wide, almost all of them writes that
// legitimately record who owns the new row. That is a different question from
// the one this gate asks, which is row SELECTION: does this construct pick rows
// by a user-scope column without checking the caller? Restricting to the filter
// clause asks exactly that, and takes the corpus from 43 matches to 1.
//
// A struct-form filter is a single line -- the parser rejects a multi-line
// clause -- so line extraction is sufficient here.
// The update block is tracked by BRACE DEPTH, not by the first line that
// trims to `}`. A nested object closes with its own `}`, so the naive version
// left the block early and every `id:` after a nested field became invisible
// (memql#2840 review). That is not a corner case: it is the shape of
// `toggleComputerUseEnabled`, the construct that opened #2840, with two lines
// swapped --
//
//	update {
//	  preferences: { computerUseEnabled: args.enabled }
//	  id: args.userId          // <- was not part of the selection surface
//	}
//
// A same-line `update { id: args.x` opener is handled too: the remainder after
// the brace is scanned like any other line, so the one-line spelling is not an
// escape hatch either.
func RowSelectionSurface(body string) string {
	body = BlankComments(body)
	var b strings.Builder
	depth := 0
	awaitingOpen := false

	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)

		if strings.HasPrefix(t, "filter") {
			b.WriteString(t)
			b.WriteByte('\n')
			continue
		}

		rest := t
		switch {
		case depth > 0:
			// already inside the update block
		case awaitingOpen:
			open := strings.IndexByte(StructureOf(t), '{')
			if open < 0 {
				continue // still between `update` and its `{`
			}
			awaitingOpen = false
			depth = 1
			rest = strings.TrimSpace(t[open+1:])
		case strings.HasPrefix(t, "update"):
			if open := strings.IndexByte(StructureOf(t), '{'); open >= 0 {
				depth = 1
				rest = strings.TrimSpace(t[open+1:])
			} else {
				// `update` on its own line; the `{` is on a following line.
				// The old helper handled this by flagging on the `update`
				// line alone; dropping the flag made the new version WEAKER
				// than the one it replaced (memql#2840 review round 2).
				awaitingOpen = true
				continue
			}
		default:
			continue
		}

		// Inside the update block: collect every `id:` assignment, wherever it
		// sits, then account for this line's braces.
		for _, seg := range strings.Split(rest, ",") {
			seg = strings.TrimSpace(seg)
			if strings.HasPrefix(seg, "id:") {
				b.WriteString(seg)
				b.WriteByte('\n')
			}
		}
		// Braces are counted on the STRUCTURE of the line -- string literals
		// blanked, comments dropped -- so `displayName: "}"` or a trailing
		// `// ends with }` cannot close the block early.
		s := StructureOf(rest)
		depth += strings.Count(s, "{") - strings.Count(s, "}")
		if depth <= 0 {
			depth = 0
		}
	}
	return b.String()
}

// MaxPredicateNesting bounds the recursion in the clause walkers. Nothing in
// the corpus approaches it; it exists so a malformed clause cannot spin.
const MaxPredicateNesting = 64

// SplitTopLevelOn splits on a DOUBLED connective (`||`, `&&`) at
// paren/brace/bracket depth 0, outside string literals.
//
// The distinction between the two connectives is irrelevant to the
// violation-hunting gates (a bad predicate is bad on either side of either)
// and load-bearing to classification (memql#2832): a conjunct NARROWS what a
// query returns, a disjunct WIDENS it.
func SplitTopLevelOn(s string, conn byte) []string {
	return splitTopLevelSep(s, conn, true)
}

// SplitTopLevelSingle splits on a SINGLE-character separator.
//
// The retired `,` OR separator is one character, so the doubled form above
// could never split it -- which is why the `,` arm of clauseGuaranteesAt found
// nothing and the classifier reported an OR-widened clause as owner-scoped
// (memql#3612).
func SplitTopLevelSingle(s string, conn byte) []string {
	return splitTopLevelSep(s, conn, false)
}

func splitTopLevelSep(s string, conn byte, doubled bool) []string {
	var raw []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		// An escaped quote does not end the literal. Without this, odd quote
		// parity leaves inStr stuck true for the rest of the clause and a
		// top-level connective after it goes unseen -- respelling the exact
		// hole this gate closes (`name=="a\"b" || ownerUserId==actor.userId`).
		case inStr && c == '\\' && i+1 < len(s):
			i++
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case depth == 0 && c == conn && doubled && i+1 < len(s) && s[i+1] == conn:
			raw = append(raw, s[start:i])
			i++
			start = i + 1
		case depth == 0 && c == conn && !doubled:
			raw = append(raw, s[start:i])
			start = i + 1
		}
	}
	return append(raw, s[start:])
}

// DelimitersBalanced reports whether every bracket and string literal in s
// closes. An authz gate must refuse to reason about text that does not.
func DelimitersBalanced(s string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case inStr && c == '\\' && i+1 < len(s):
			i++
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !inStr
}

// StripLeadingNot removes a negation prefix. `!=` is a comparison operator,
// never a leading negation, so it is left alone.
func StripLeadingNot(p string) (string, bool) {
	if !strings.HasPrefix(p, "!") || strings.HasPrefix(p, "!=") {
		return p, false
	}
	return strings.TrimSpace(p[1:]), true
}

// StripOuterParens peels a paren pair wrapping the WHOLE predicate. It declines
// when the opener closes early (`(a) == (b)`), which is a comparison between
// groups rather than one parenthesized predicate.
func StripOuterParens(p string) (string, bool) {
	if len(p) < 2 || p[0] != '(' || p[len(p)-1] != ')' {
		return p, false
	}
	depth := 0
	inStr := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 && i != len(p)-1 {
				return p, false
			}
		}
	}
	if depth != 0 {
		return p, false
	}
	return strings.TrimSpace(p[1 : len(p)-1]), true
}

// UnwrapWhenPredicate returns the inner predicate of a `when(args.x) { <inner> }`
// guard, or the predicate unchanged when it is not a guard.
func UnwrapWhenPredicate(p string) string {
	// The lexer is token-based, so `when (args.x) { ... }` with a space is
	// legal MemQL, and memqlfmt does not normalise it away (it is a lexical
	// formatter). A HasPrefix(p, "when(") test missed that spelling entirely,
	// which for the guard rule in ClauseGuarantees silently turned a
	// CONDITIONAL predicate back into a guarantee (memql#2832).
	rest, isWhen := strings.CutPrefix(p, "when")
	if !isWhen || !strings.HasPrefix(strings.TrimLeft(rest, " \t"), "(") {
		return p
	}
	open := strings.Index(p, "{")
	closeIdx := strings.LastIndex(p, "}")
	if open < 0 || closeIdx <= open {
		return p
	}
	return strings.TrimSpace(p[open+1 : closeIdx])
}

// ClauseGuarantees reports whether EVERY row a filter clause can admit
// satisfies `leaf` -- i.e. whether the guarantee holds on all paths through
// the clause's boolean structure, not merely somewhere in its text.
//
// The rules follow from what each connective does to a result set:
//
//   - DISJUNCTION widens. `A || B` guarantees the property only if BOTH arms
//     do; one unscoped arm returns rows the property does not cover.
//   - CONJUNCTION narrows. `A && B` guarantees it if EITHER conjunct does;
//     the other can only remove rows.
//   - NEGATION inverts. `!A` is never a guarantee -- `!(ownerUserId ==
//     actor.userId)` is precisely "rows I do not own".
//   - A `when(args.x) { A }` guard is CONDITIONAL: when the arg is absent the
//     predicate is dropped as if never written, so it cannot guarantee
//     anything on its own.
//
// Parens are peeled before each test so `(A || B) && C` is read as a
// conjunction, not as text containing `||`.
func ClauseGuarantees(clause string, leaf func(string) bool) bool {
	return clauseGuaranteesAt(clause, leaf, 0)
}

func clauseGuaranteesAt(clause string, leaf func(string) bool, depth int) bool {
	if depth > MaxPredicateNesting {
		return false // Unreadable structure never counts as a guarantee.
	}
	s := strings.TrimSpace(clause)
	// Refuse to reason about text that does not close cleanly. The
	// struct-query parser rejects such a clause long before the gate sees it,
	// so this is unreachable today -- but a depth counter with no floor reads
	// `) || ownerUserId==actor.userId` as one scoped predicate, and failing
	// OPEN is the wrong default for an authz gate.
	if depth == 0 && !DelimitersBalanced(s) {
		return false
	}
	if inner, ok := StripOuterParens(s); ok {
		return clauseGuaranteesAt(inner, leaf, depth+1)
	}
	if s == "" {
		return false
	}
	if arms := SplitTopLevelOn(s, '|'); len(arms) > 1 {
		for _, a := range arms {
			if !clauseGuaranteesAt(a, leaf, depth+1) {
				return false
			}
		}
		return true
	}
	// The retired `,` separator is a pure alias for `||` in the engine, at the
	// same OR precedence -- so it is split HERE, before '&', or `a && b, c`
	// would be read as a conjunction (memql#3612).
	//
	// Without this arm the function fell through to a leaf check on the whole
	// joined text, found the `actor.userId` substring, and reported
	// `(ownerUserId==actor.userId, visibility=="public")` OWNER-SCOPED -- while
	// the engine returned every public row regardless of owner. An
	// authorization bypass produced by an operator nobody expected to still
	// work. HasTopLevelComma now refuses the spelling outright; this is the
	// second lock, because a classifier that cannot see an operator the engine
	// honours is wrong whether or not another gate happens to catch it first.
	if arms := SplitTopLevelSingle(s, ','); len(arms) > 1 {
		for _, a := range arms {
			if !clauseGuaranteesAt(a, leaf, depth+1) {
				return false
			}
		}
		return true
	}
	if arms := SplitTopLevelOn(s, '&'); len(arms) > 1 {
		for _, a := range arms {
			if clauseGuaranteesAt(a, leaf, depth+1) {
				return true
			}
		}
		return false
	}
	if _, isNot := StripLeadingNot(s); isNot {
		return false
	}
	if inner := UnwrapWhenPredicate(s); inner != s {
		return false
	}
	return leaf(s)
}

// FilterClauseOf returns a struct-form construct's `filter` clause with its
// boolean structure intact, or "" when the construct has none.
//
// It exists separately from every other filter extractor in the repo because
// those FLATTEN the clause into a predicate list, and flattening is exactly
// what classification cannot use: see ClauseGuarantees.
func FilterClauseOf(body string) string {
	var out []string
	inFilter := false
	for _, line := range strings.Split(BlankComments(body), "\n") {
		trim := strings.TrimSpace(line)
		if !inFilter {
			if rest, ok := strings.CutPrefix(trim, "filter"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
				inFilter = true
				out = append(out, strings.TrimSpace(rest))
			}
			continue
		}
		if trim == "" || strings.HasPrefix(trim, "@") || strings.HasPrefix(trim, "}") || IsClauseEndKeyword(trim) {
			break
		}
		out = append(out, trim)
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

// IsClauseEndKeyword reports whether a line starts the next clause of a
// struct-form body, terminating the filter. Delegates to the shared keyword
// set (memql#2815) so each gate does not spell the list again, slightly
// differently, with nothing comparing them.
func IsClauseEndKeyword(trim string) bool {
	return dslclause.StartsAnyOf(trim, dslclause.BodyKeywords)
}
