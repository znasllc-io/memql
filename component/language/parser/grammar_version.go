package parser

// Grammar-version stamp (S6, memql#2361). Nothing previously recorded which
// grammar a .memql file, pack, or authored-bundle row was written against, so
// a construct authored under an older grammar degraded to a boot Warn/Debug
// instead of a detectable, actionable mismatch (the product pack rotted
// exactly this way, bff#153).
//
// GrammarVersion is bumped ON EVERY GRAMMAR EPIC -- a change that retires or
// reshapes an authored form (new invocation syntax, retired annotation,
// payload-binding change).
//
// # When a bump must ship a rewrite mode, and when it need not (memql#3089)
//
// The contract used to read "each bump MUST ship a `memqlmigrate
// --rewrite=<epic>` mode", with no exception. Six narrowings landed under it
// and none shipped one, which is the shape of a rule nobody can follow rather
// than a rule everyone ignored: a `--rewrite` mode mechanically migrates
// AUTHORED SOURCE, and a narrowing whose retired form appears in no authored
// source has nothing to rewrite. Writing an empty codemod to satisfy the letter
// of the contract would add a migration channel that migrates nothing and still
// has to be maintained.
//
// The rule, amended to what is actually followable:
//
//   - A bump MUST ship a `memqlmigrate --rewrite=<epic>` mode when the retired
//     form can appear in authored source a consumer may still hold -- the
//     in-tree `dsl/` tree, a product DSL bundle, or a durably-promoted
//     v1:authoring:construct row.
//   - A bump need NOT ship one when the retired form provably appears in none
//     of those. The bump is still REQUIRED: the stamp is what lets a durable row
//     authored under the old grammar be quarantined with a diagnostic instead of
//     a raw parse error, and that value does not depend on a codemod existing.
//
// Either way, say which arm applies in the commit, with the measurement that
// supports it. "No in-tree usage" is a claim about a corpus, it is cheap to
// check, and it was checked for none of the six.
//
// The codemod-per-epic pattern remains the published migration channel
// (docs/public/language/authoring-rules.md).

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// GrammarVersion names the current grammar epoch. Format: <year>.<month>-<epic-slug>.
//
// Bumped 2026-08-05 (memql#3089). The constant had not moved since cb62512c
// (2026-07-21) while SIX grammar moves landed, so the durable-rehydration stamp
// guard in component/memql/authoring_promote_durable.go compared equal every
// time and never fired. The narrowings this bump covers:
//
//	83574995  2026-08-03  reject a repeated annotation argument (#2968)
//	d53bad46  2026-07-21  bury @role
//	489a414b  2026-07-21  bury @permission
//	0d13dd96  2026-07-21  retire construct-level @internal
//	93b365ed  2026-07-21  hard-retire eight expression builtins
//	6e7d09ac  2026-08-02  add the `asOf args.X` clause form (#3025)
//	#3085     2026-08-05  remove the bare `asOf args.X` form again (#3028)
//
// No `--rewrite` mode ships with this bump, under the second arm of the rule
// above: none of the retired forms appears in the in-tree `dsl/` corpus,
// measured rather than assumed (`dsl/no_retired_annotations_test.go` and
// `dsl/no_retired_builtins_test.go` gate that corpus and are green, and the bare
// `asOf` form existed only between #3025 and #3085).
const GrammarVersion = "2026.08-annotation-and-builtin-narrowings"

// GrammarFingerprint is a drift detector over the author-facing grammar
// SURFACE: when any axis below changes, the pinned test fails and forces a
// CONSCIOUS GrammarVersion bump.
//
// # Why it is four axes and not one (memql#3089)
//
// It used to hash the invocation-kind keyword set alone. That set is stable --
// it has not changed since the pin was written -- so the test stayed green
// through every one of the six narrowings listed above. A drift detector that
// watches only the part of the grammar nobody is changing is not a drift
// detector.
//
// The axes are taken from what actually moved, each checked against the tree
// rather than guessed:
//
//	invocation keywords  the original axis, kept
//	annotations          @role / @permission / @internal are absent from
//	                     annotations.ByReceiver, so burying them moved this axis
//	retired builtins     the eight went INTO retiredExprBuiltins
//	struct-query clauses `asOf` entered the rewriter's clause set
//
// # WHAT THIS CANNOT CATCH, stated rather than implied
//
// Every axis is a SET. A narrowing that changes a RULE while leaving every set
// identical is invisible here, and two of the seven commits above are exactly
// that shape:
//
//   - #2968 (reject a repeated annotation argument) narrowed the ARITY rule for
//     annotation arguments. No annotation name changed.
//   - #3028 (remove the bare `asOf args.X` form) narrowed what may FOLLOW a
//     clause keyword. `asOf` is still a clause.
//
// So this catches set-shaped narrowings and misses rule-shaped ones: 5 of the 7
// commits above would have tripped it, 2 would not. It is a floor, not a proof,
// and saying so is the point -- a reviewer who reads it as "the fingerprint will
// tell me" will ship a precedence or arity change unbumped. For those the bump
// stays a human step, which is why the contract says every grammar EPIC bumps,
// not every fingerprint change.
func GrammarFingerprint() string {
	var parts []string
	for _, axis := range grammarSurfaceAxes() {
		parts = append(parts, strings.Join(axis, ","))
	}
	return hashJoined(parts)
}

// hashJoined is the one hashing step, shared with the axis test so that test
// measures the SHIPPED composition rather than a second copy of it. A pin test
// carrying its own hashing is a pin over a string that exists only inside
// itself -- the failure mode already documented on this repo's other gates.
func hashJoined(parts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

// grammarSurfaceAxes returns the fingerprinted axes in a stable order, each
// sorted. Exposed to the test so every axis can be proven load-bearing: an axis
// that contributes nothing to the hash is a lane that reads as covered and is
// not, which is the same defect memql#3089 is about one level up.
func grammarSurfaceAxes() [][]string {
	return [][]string{
		invocationKeywordAxis(),
		annotationAxis(),
		retiredExprBuiltinAxis(),
		structQueryClauseAxis(),
	}
}

func invocationKeywordAxis() []string {
	kws := InvocationKindKeywords()
	sort.Strings(kws)
	return kws
}

// annotationAxis pairs each annotation with the receiver that accepts it, so
// MOVING an annotation between receivers registers as drift too. That is a
// change to what an author may write on a construct, which is exactly what this
// stamp exists to date.
func annotationAxis() []string {
	out := make([]string, 0, 64)
	for receiver, names := range annotations.ByReceiver {
		for _, name := range names {
			out = append(out, receiver+"."+name)
		}
	}
	sort.Strings(out)
	return out
}

func retiredExprBuiltinAxis() []string {
	out := make([]string, 0, len(retiredExprBuiltins))
	for name := range retiredExprBuiltins {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func structQueryClauseAxis() []string {
	out := append([]string(nil), structQueryClauseKeywords...)
	sort.Strings(out)
	return out
}
