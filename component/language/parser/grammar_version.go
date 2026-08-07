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
// # Narrowings this bump covers
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
const GrammarVersion = "2026.08-asof-fallback-and-annotation-arg-narrowings-c0eedce6"

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
