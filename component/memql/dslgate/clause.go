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

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslclause"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// ConstructHeaderRe matches the header line of a row-accessing construct.
//
// The keyword set is `query|mutation|seed`. It was `query|mutate|seed`:
// memql#3013 renamed `mutation` -> `mutate` and epic memql#5375 renamed it
// BACK, and the hazard is the same in both directions -- a classifier looking
// for the word the corpus no longer writes matches nothing and reports a clean
// tree. That is what happened here twice. A classifier looking for
// `mutation` walked queries only and every mutation and seed in the tree was
// invisible to it. component/language/dslspec already hard-fails if `mutation`
// is still a construct keyword; the classifier never got the memo.
var ConstructHeaderRe = regexp.MustCompile(`(?m)^[ \t]*(query|mutation|seed)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// UserScopeFieldRe matches a user-scope field named BARE, as an update block's
// `id:` line names it. A filter's selection is read on its tree instead
// (v1ReadsUserScopeField): there every field is a member of the parameter,
// `row.ownerUserId`, which this pattern deliberately cannot see.
//
// A detector that searches for a spelling the corpus has migrated off reports
// a clean corpus rather than failing: its predecessor looked for the retired
// `payload.`-prefixed form, reported 0 flagged across ~198 constructs, and
// that zero read as "audited and clean" rather than "not measuring what its
// name says" (memql#2799).
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
// must be a term that is false for a non-admin. `actor.isClusterOwner != true`
// and `== false` contain the same identifier and invert the meaning -- under
// them a non-owner who satisfies the other conjunct gets rows and the cluster
// owner gets none, which is the very failure this gate exists to refuse. `!=`
// is a comparison, not a negation, so the tree's negation rule does not catch
// it either: the leaf's own text must.
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
//
// THREE NAMES LEFT THIS PATTERN IN EPIC memql#5166, and their absence is the
// point rather than an omission. `requiresAdmin`, `requiresOwnerOrAdmin` and
// `requiresDeveloperOrAbove` compared the actor's role STRING against one or
// three literals, which cannot see a custom role at all -- `role == "admin"` is
// false for a rank-250 role holding every principal verb. They are deleted from
// dsl/common/specs.memql, and the nine constructs that named them now carry
// `@requiresRank` or `@requiresCapability` instead.
//
// AN ANNOTATION IS NOT A FILTER LEAF, so this pattern cannot see one and must
// not pretend to. ConstructCarriesActorGate below is the question to ask about
// a construct's HEAD; this stays the question about a filter's leaves, and the
// callers that decide "is this construct caller-gated" must ask both.
var AdminGateRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner[ \t]*==[ \t]*true|requiresClusterOwner|requiresOwner|forgeApprover|forgeDeveloper)([^A-Za-z0-9_]|$)`)

// AdminGateMentionRe is the POLARITY-BLIND twin, and the two must stay
// separate: selection has to be broad and assertion strict.
//
// If the gate selected constructs with the strict predicate, an inverted
// filter (`actor.isClusterOwner!=true`) would simply not be recognised as
// admin-gated, get skipped, and sail through -- swapping one fail-open for
// another. Selecting on any MENTION and then demanding the strict form is what
// turns the inverted spelling into an error instead of a silence.
var AdminGateMentionRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner|requiresClusterOwner|requiresOwner|forgeApprover|forgeDeveloper)([^A-Za-z0-9_]|$)`)

// ActorGateAnnotationRe matches the SERVER-SIDE SURFACE gates a construct
// declares above its signature: `@requiresRank("<slug>")` (a floor on the
// cluster's ladder, epic memql#4832) and `@requiresCapability("<verb>",
// "<resource>")` (a grant a role holds, epic memql#5166).
//
// THEY ARE CALLER GATES AND THEY LIVE IN THE HEAD, which is why they need a
// pattern of their own. Every gate this package knew until now was a filter
// LEAF, so a caller check moving out of the filter and onto the construct reads
// to every one of those checks as a gate that simply vanished -- which is
// exactly what the four conformance gates reported when the nine constructs
// migrated. A recogniser that cannot see a gate treats the construct as
// ungated, and the composition rule never runs on it: that is the failure mode
// this file's own header describes, arriving from the other direction.
//
// STRICTER THAN THE FILTER GATES IN ONE WAY: there is no polarity to get wrong.
// An annotation cannot be negated or put inside a disjunction, which is the
// whole reason D11 chose the annotation form over another spec.
var ActorGateAnnotationRe = regexp.MustCompile(
	`@requires(?:Rank|Capability)\s*\(`)

// ConstructCarriesActorGate reports whether a construct's annotation HEAD --
// the text between the previous construct and this one's signature -- declares
// an enforced actor gate.
//
// Pass the head, not the whole file: an annotation belonging to the construct
// above would otherwise read as this one's, which is the same mistake the
// filter-leaf gates avoid by taking a single clause.
func ConstructCarriesActorGate(head string) bool {
	return ActorGateAnnotationRe.MatchString(head)
}

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
// THE FILTER IS THE WHOLE CLAUSE, continuation lines included. This used to
// say a struct-form filter is a single line because the parser rejected a
// multi-line clause; memql#4123 made the normaliser fold continuation lines,
// and this kept reading the first line only -- so a user-scope column moved
// onto a wrapped `&& ownerUserId==args.x` line was outside the surface. The
// edition-2026 codemod wraps every long filter that way (epic memql#5363),
// which would have made it the common case rather than a corner.
//
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
	clause, ids := rowSelectionParts(body)
	var b strings.Builder
	if clause != "" {
		b.WriteString("filter  ")
		b.WriteString(clause)
		b.WriteByte('\n')
	}
	for _, id := range ids {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	return b.String()
}

// SelectsByUserScopeField reports whether a construct body SELECTS rows by a
// user-scope column: whether its row-selection surface (RowSelectionSurface)
// reads one of UserScopeFields as the row's own column.
//
// The filter is read as a tree. Every field is a member of the lambda
// parameter -- `row.ownerUserId == args.x` -- so the selection is a member
// chain rooted at the parameter whose first field is a user-scope column
// (v1ReadsUserScopeField). A clause that does not parse is read as text,
// looking for `<param>.<field>`: the loader refuses it with the parser's own
// message, and what this gate must not do is pass over a user-scope selection
// because the parse failed. A clause with no lambda header at all is the
// retired `filter <predicate>` form, which the parser refuses at load and the
// retired-operator gate reports.
func SelectsByUserScopeField(body string) bool {
	clause, ids := rowSelectionParts(body)
	if lam, isV1, err := v1Clause(clause); isV1 {
		switch {
		case err != nil || lam == nil:
			if v1TextReadsUserScopeField(clause) {
				return true
			}
		case v1ReadsUserScopeField(lam):
			return true
		}
	}
	for _, id := range ids {
		if UserScopeFieldRe.MatchString(id) {
			return true
		}
	}
	return false
}

// rowSelectionParts returns a body's filter clause (FilterClauseOf) and every
// `id:` assignment of its update block.
func rowSelectionParts(body string) (clause string, ids []string) {
	body = BlankComments(body)
	clause = FilterClauseOf(body)
	depth := 0
	awaitingOpen := false

	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)

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
				ids = append(ids, seg)
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
	return clause, ids
}

// ClauseGuarantees reports whether EVERY row a filter clause can admit
// satisfies `leaf` -- i.e. whether the guarantee holds on all paths through
// the clause's boolean structure, not merely somewhere in its text.
//
// The clause is read as a tree (ast.Guarantees over the parsed lambda body),
// and the rules follow from what each connective does to a result set:
//
//   - DISJUNCTION widens. `A || B` guarantees the property only if BOTH arms
//     do; one unscoped arm returns rows the property does not cover.
//   - CONJUNCTION narrows. `A && B` guarantees it if EITHER conjunct does;
//     the other can only remove rows.
//   - NEGATION inverts. `!A` is never a guarantee -- `!(row.ownerUserId ==
//     actor.userId)` is precisely "rows I do not own".
//
// The optional-argument guard needs no rule of its own: `(args.x == nil || e)`
// is a disjunction whose first arm guarantees nothing, so it is conditional --
// when the argument is absent the predicate admits every row -- and
// `(args.x != nil && e)` under a `||` admits only rows e admits.
//
// Each leaf is judged by `leaf` over its canonical source with string contents
// blanked (v1LeafText), so a quoted word is never read as a reference and the
// vocabulary of what counts as an owner or admin check is one list of text
// predicates. A clause that does not parse, that binds other than one
// parameter, or whose parameter shadows a reserved root (`actor => ...` would
// make `actor.userId` a ROW field) guarantees nothing -- and so does a clause
// with no lambda header, the retired `filter <predicate>` form the parser
// refuses at load. Unreadable structure never counts as a guarantee.
func ClauseGuarantees(clause string, leaf func(string) bool) bool {
	lam, isV1, err := v1Clause(clause)
	if !isV1 || err != nil || lam == nil || len(lam.Params) != 1 || isReservedRoot(lam.Params[0]) {
		return false
	}
	return ast.Guarantees(lam.Body, func(n ast.ExpressionNode) bool { return leaf(v1LeafText(n)) })
}

// FilterClauseOf returns a struct-form construct's `filter` clause with its
// boolean structure intact, or "" when the construct has none.
//
// It exists separately from every other filter extractor in the repo because
// those FLATTEN the clause into a predicate list, and flattening is exactly
// what classification cannot use: see ClauseGuarantees.
//
// Which lines belong to the clause is dslclause.ClauseExtent's answer -- the
// fold the struct-query normaliser applies -- so the text classified is the
// text the engine runs. It used to run to the next blank line, annotation,
// brace or keyword, which agreed with the normaliser on the clauses it had met
// and differed on others: a blank line inside a wrapped clause ended it here
// and not there, so a conjunct after the blank was invisible to the classifier
// while the engine applied it.
func FilterClauseOf(body string) string {
	lines := strings.Split(BlankComments(body), "\n")
	for i, line := range lines {
		if isFilterOpener(strings.TrimSpace(line)) {
			parts, _ := filterClauseLines(lines, i)
			return joinClauseLines(parts)
		}
	}
	return ""
}

// isFilterOpener reports whether a trimmed, comment-free line opens a
// `filter` clause.
func isFilterOpener(trim string) bool {
	rest, ok := strings.CutPrefix(trim, "filter")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// filterClauseLines returns the filter clause that opens on lines[i], one
// entry per physical line: the text after the keyword, then every line
// dslclause.ClauseExtent folds into it, each trimmed. A blank line inside the
// clause is an empty entry, so an entry's index is its line offset from i and
// a position the v1 parser reports maps back onto the source. last is the
// index of the clause's final line. lines must be comment-free.
func filterClauseLines(lines []string, i int) (parts []string, last int) {
	last = dslclause.ClauseExtent(lines, i)
	parts = append(parts, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "filter")))
	for j := i + 1; j <= last; j++ {
		parts = append(parts, strings.TrimSpace(lines[j]))
	}
	return parts, last
}

// joinClauseLines renders a clause's lines as one line of text.
func joinClauseLines(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// IsClauseEndKeyword reports whether a line starts the next clause of a
// struct-form body, terminating the filter. Delegates to the shared keyword
// set (memql#2815) so each gate does not spell the list again, slightly
// differently, with nothing comparing them.
func IsClauseEndKeyword(trim string) bool {
	return dslclause.StartsAnyOf(trim, dslclause.BodyKeywords)
}
