package parser

// Grammar-version stamp (S6, memql#2361). Nothing previously recorded which
// grammar a .memql file, pack, or authored-bundle row was written against, so
// a construct authored under an older grammar degraded to a boot Warn/Debug
// instead of a detectable, actionable mismatch (the product pack rotted
// exactly this way, bff#153).
//
// GrammarVersion is bumped ON EVERY GRAMMAR EPIC -- a change that retires or
// reshapes an authored form (new invocation syntax, retired annotation,
// payload-binding change, a new or removed struct-body clause).
//
// # When a `memqlmigrate --rewrite=<epic>` mode is required (amended, memql#3089)
//
// The original contract said every bump MUST ship one. That rule was stated and
// then broken six times in six weeks, which is evidence about the rule as much
// as about the commits. Amended to what is actually defensible:
//
// A rewrite mode is REQUIRED when a narrowing can strand source someone else
// holds -- that is, when the retired form has in-tree usage, or plausible usage
// in a MEMQL_DSL_PATH bundle or a durably-promoted `v1:authoring:construct` row.
// Then the codemod is the published migration channel
// (docs/public/language/authoring-rules.md) and authors are entitled to it.
//
// A rewrite mode is NOT required for a narrowing with no in-tree usage and no
// stored-row exposure, nor for a WIDENING (nothing previously valid stopped
// being valid). Both still bump the version: the version records what the
// grammar IS, not whether anyone was inconvenienced.
//
// # The bump is now enforced, and it is now safe
//
// ENFORCED: GrammarVersion must end with the 8-hex digest of the authored
// surface, computed in grammar_surface_drift_test.go from a behavioural
// accept/reject corpus, the struct-body parsers' own clause `case` arms, and the
// invocation-keyword set. Change what an author may write and the digest moves;
// nothing in that test is hand-pinned, so the ONLY edit that restores green is
// to this constant. That closes the hole the previous four-word denylist left
// open on both sides: it could not see a new clause, and re-pinning its literal
// restored green with the version untouched.
//
// SAFE: the durable-rehydration stamp guard was inverted in the same issue
// (component/memql/authoring_promote_durable.go). It now attempts the recompile
// FIRST and uses a stale stamp only to explain a failure, so a bump can no
// longer unregister a stored construct whose source still parses. Mandatory
// bumping was unaffordable while a bump quarantined every stored row, and that
// -- not carelessness alone -- is why the constant stopped moving.
//
// # Narrowings the 2026.09-dsl-v1-foundations bump covers (memql#5359)
//
// The annotation registry (component/language/annotations) became the one
// annotation gate, at PARSE time, for every construct and field list. Each
// form below parsed on the authored path (NormaliseAll + ParseFile) before it
// and is refused now, with an `annotation_*` code; the corpus in
// grammar_surface_drift_test.go carries one entry per kind. None has in-tree
// usage -- dsl/, examples/ and both local product bundles lint clean -- except
// two corpus defects removed in the same change (an orphaned @actor("system")
// in dsl/platform/mutations.memql, and two orphaned @sdk lines that stacked
// three @sdk onto one builtin), so no rewrite mode ships:
//
//   - an unknown annotation on a prompt field, a builtin field, an action or a
//     capability (the four places nothing checked);
//   - an unknown annotation on a query, mutation, logic or automation, now
//     refused by the parser where only a load-time text scan of the names
//     refused it before;
//   - an annotation written in an argument form its receiver does not take:
//     a flag given an argument (`@serverOnly("yes")`), `@cache("300")`, a bare
//     `@when` on a rule, an `@args({...})` object on a builtin, a number or a
//     bare word as a tool field's `@default`, and every other form the
//     registry's placement does not list;
//   - an unknown keyword key (`@trigger(evnt=...)`);
//   - a keyword key written in the shape its placement does not take: a
//     valued key written bare (`@rateLimit(maxCalls, periodSeconds)`, which
//     registered the tool with no limit; `@cache(ttl)`) or a flag key given a
//     value (`@rowAuthz(clusterOwner="x")`);
//   - a non-repeatable annotation written twice;
//   - a clause a logic, automation or mutation body does not take: the logic
//     and automation emitters kept the clauses they recognised and dropped
//     everything else (a `filter` line in an automation, a `step` block in a
//     logic), and the mutation body dropped a top-level line that was
//     neither a block nor a field. A `step` or `precondition` block without
//     its name, and a second `args` or `body` block in a logic or
//     automation, are refused with them.
//
// `@trigger(on=<concept>.<event>)` stays legal: the synonym for event= that
// the automation loader and the concept resolver fold, which the first cut of
// the registry had refused.
//
// Four narrowings happen at LOAD, in the concept translator
// (component/database), not on this path, so the digest cannot record them;
// the concept tests pin them instead: the key-shape rule for a concept's
// keyword annotations (@rowAuthz, @composable, @displayCard, @relationship,
// @variant); an unknown @relationship key, which the translator used to
// ignore; the repeat rule on a concept's annotations; and the field spellings
// its readers accepted and dropped -- a `value=` keyword on a numeric bound or
// a description (`@minLength(value=5)`), a quoted number (`@minimum("5")`), and
// a bare word other than true/false as a @default (`@default(open)`).
//
// One more narrowing happens at LOAD, in the construct-keyword gate
// (FindUnknownConstructKeywords, construct_unknown), so the digest cannot
// record it either: the file-top `import ( ... )` block. It loaded on the
// engine before this epic (8a063ec3f) -- no loader ever read it -- and is
// refused now, by name, naming its replacement, a file-top
// `use <domain>.<file>.{ names }` line. This path still parses the block
// (dslimports builds its import graph from it), which is why the corpus case
// test/conformance/2026/negative/use/import-block.memql pins it as
// refuse_load. No rewrite mode ships: the tree has no usage, and a `use` line
// names constructs where the block named files, so the replacement is not
// mechanical.
//
// # Narrowings the 2026.08 bump covered
//
// The constant last moved in cb62512c (2026-07-21). Everything below reshaped an
// authored form without bumping it, and is recorded here rather than migrated:
// none has in-tree usage, and the bare `asOf args.X` window is the only one with
// any durable-row exposure at all.
//
//   - 83574995 (2026-08-03, memql#2968) -- a repeated annotation argument is
//     rejected instead of collapsing last-wins. A previously-parsing form errors.
//   - 6e7d09ac (2026-08-02, memql#3025) ADDED the bare `asOf args.X` grammar;
//     memql#3028 / #3085 REMOVED it again, requiring `?? latest`. Exposure is
//     confined to rows authored inside that window; the tree uses the fallback
//     form.
//   - d53bad46 / 489a414b / 0d13dd96 (2026-07-21) -- @role buried, @permission
//     buried, @internal retired.
//   - 93b365ed (2026-07-21, memql#2707) -- eight zero-use expression builtins
//     hard-retired (year, quarter, month, dayOfMonth, isAnniversary,
//     isFirstDayOfQuarter, memqlVersion, subtractTimestamps).
//
// # 2026.09-dsl-v1-expressions (memql#5364)
//
// The edition-2026 predicate positions, accepted BESIDE the legacy spellings
// until the flip that came with the tree's migration: a struct
// query's `filter row => ...`, `spec <bound> <name> = row => ...`,
// `trait <name> = row => ...`, `@filter(row => ...)` (also inline on a terse
// automation header, and as @trigger's filter=), and the new `refine <lambda>`
// clause, legal only with `paginate`. Every one is a WIDENING, so no rewrite
// mode is owed for this bump; memqlmigrate --rewrite=expressions is what the
// later flip ships.
//
// One narrowing: a call named `refine(...)` in the internal query form is now
// the refine directive, which takes refine(paginate(...), row => ...). No
// construct in the tree calls a function by that name.
//
// Not a surface change, recorded because it changes an internal string: a
// struct query's filter joins its concept as `concept==<id> && (<filter>)`,
// not `concept==<id>;<filter>`. The `;` bound at `&&` level, so a filter whose
// top level was an `||` split around it; every such filter in the tree was
// already parenthesised, so no shipped query changes meaning.
//
// # The edition-2026 flip (memql#5364, memql#5368)
//
// The tree's migration made the edition-2026 expression grammar the only
// authoring grammar, so every authored position parses it; the switch that
// chose between the two grammars while the tree migrated is gone. The
// internal query form -- ParseExpression, the string an SDK sends to Execute
// -- keeps its grammar; nothing below reaches it.
//
// NARROWINGS. Each parsed on the authored path before the flip and is refused
// now, naming its replacement. The tree used them, and was migrated in the
// same change, so this bump owes a rewrite mode and ships it: memqlmigrate
// --rewrite=expressions (dsl/ and the product bundles), with
// scripts/migrations/expressions_go_fixtures for Go test fixtures.
// V1RetiredForms (v1_refusals.go) is the list with rule ids; by position:
//
//   - the predicate positions' legacy spellings: a `filter` with no lambda
//     header, a spec or trait `{ return ... }` body, a raw-text @filter, and
//     @trigger's filter= written as a string;
//   - in a logic statement, a mutation value, a step's arguments and its
//     condition: the calls an operator or a method replaces -- cond(),
//     concat(), coalesce(), exists(), len(), count(), mean(), first(), last(),
//     and(), or(), lt(), gt(), lte(), gte() -- and `null`, `.contains(...)`, a
//     key-less map entry (`{ args.x }`), the `;` connective and `has`;
//   - two spellings the v1 grammar refuses outside that table: a named
//     argument written `name=value` (it is `name: value`) and a quoted map key
//     (`{"k": 1}`, authoring rule 18);
//   - a canonical id written bare in an expression (`concept == v1:crm:lead`):
//     it is a string, and is written quoted.
//
// Not narrowings, because the legacy grammar refused them too: not(),
// timestamp(), now() and `$args.x`. The #2707 builtins, caller() and an
// `asOf` outside a query keep the refusals the legacy grammar gave them; the
// v1 call parser repeats each, so none reads as a call to an undefined
// function.
//
// WIDENINGS. A step's right-hand side is any expression -- `n := 5` is a
// query step the runtime evaluates, where the legacy grammar required a call
// or a collection chain -- and a member read may be optional, `x.?field`.
//
// A durably-promoted `v1:authoring:construct` row written in a retired
// spelling stops recompiling at this version; the inverted stamp guard names
// the stale stamp as the reason, and memqlmigrate --rewrite=expressions is
// the way back.
//
// # 2026.09-dsl-v1-loop-protection (memql#5381)
//
// A WIDENING: an automation takes two annotations it refused before as
// annotation_unknown. `@loop(maxDepth=N, until=row => P)` permits a
// deliberate cycle, bounds it to N runs of the automation per causal chain,
// and names the predicate that ends it; `@mode(single | queued | restart |
// parallel[, max=N])` says how concurrent fires of one automation behave. The
// registry holds their keys (annotations.Placements); until= parses the
// one-parameter lambda @trigger's filter= parses.
//
// The parser refuses a value its source alone shows is wrong -- an until that
// is not a lambda of one parameter (loop_until_form), a maxDepth that is not
// a whole number or is left out (loop_max_depth_range), a max below 1
// (mode_max_range) -- and none of those spellings parsed before either, since
// the annotation did not. The load refuses the rest (component/automations,
// loop_prepare.go): maxDepth past the depth cap, an until the @filter does not
// exclude, a @loop on an automation no event triggers, and a @mode naming no
// mode or two. Nothing previously valid stopped being valid, so no rewrite
// mode is owed.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// GrammarVersion names the current grammar epoch.
// Format: <year>.<month>-<epic-slug>-<8 hex authored-surface digest>.
//
// The digest suffix is not decoration: TestGrammarVersionCarriesTheSurfaceDigest
// recomputes it and requires this string to end with it, which is what makes a
// grammar move impossible to land without editing this line (memql#3089).
const GrammarVersion = "2026.09-dsl-v1-loop-protection-930e046b"

// GrammarFingerprint is a drift detector over the author-facing keyword
// surface: when the invocation-kind keyword set changes, the pinned test
// fails and forces a CONSCIOUS GrammarVersion bump (plus the migration mode
// that must accompany it).
func GrammarFingerprint() string {
	kws := InvocationKindKeywords()
	sort.Strings(kws)
	sum := sha256.Sum256([]byte(strings.Join(kws, "|")))
	return hex.EncodeToString(sum[:8])
}
