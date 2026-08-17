package memql

// rowauthz_source_rows.go -- the per-row authorization gate, exported for the
// capabilities that read the node store DIRECTLY and hand back something OTHER
// than the rows they read (memql#4029).
//
// # The hole this closes: a REPACK defeats a concept-keyed gate by construction
//
// Both of the engine's row-authz mechanisms resolve the tier from a concept:
//
//	filter injection -- from plan.BoundConcept    (rowauthz_enforce.go)
//	the row gate     -- from the row's OWN concept (rowAuthzAdmits)
//
// memql#3982 closed the top-level-builtin seam and its fix works because every
// row that seam returns carries its real concept. A capability that READS real
// rows and RETURNS a synthetic summary of them breaks that premise. Measured on
// `integration.chat.recentChat`: it reads `v1:cognition:utterance` rows and
// emits one node stamped `chat.recentChat`, whose payload carries the utterance
// TEXT. The gate is then asked about `chat.recentChat`, which declares no tier,
// and admits -- while whatever tier `v1:cognition:utterance` declares is never
// consulted, because no row bearing that concept ever reaches the gate.
//
// It fails SILENTLY and in the admitting direction: the gate runs, finds nothing
// declared, and says yes.
//
// # Why the gate goes on the SOURCE rows and not on the repacked node
//
// The obvious repair -- stamp the source concept onto the summary node so the
// existing gate resolves the real tier -- was considered and rejected. It turns
// a per-ROW decision into an all-or-nothing one, and the direction it errs is
// not even stable across tiers:
//
//   - `owned` reads the declared owner field off the row's payload. A summary
//     envelope has no such top-level field, so rowAuthzAdmits denies the whole
//     summary -- including the rows the caller does own.
//   - `granted` is undecidable from a row in isolation, so the summary is denied
//     wholesale for the same reason.
//   - a summary of rows from SEVERAL concepts (the ordinary case for a context
//     builder) has no single concept to stamp at all.
//   - the summary's id is synthesized, so the self-owned spelling of `owned`
//     resolves against a string that names nothing.
//
// The rows themselves, at the moment they come back from the query, still carry
// their real concept, their real id and their real payload. That is the only
// point on the path where the existing gate can answer correctly, so that is
// where it goes. Repacking AFTER the gate is then harmless: nothing that reached
// the summary was withheld.
//
// # Semantics: this is the fail-CLOSED gate, deliberately
//
// It delegates to admitRowAuthzBuiltinResult -- `rowAuthzAdmit` only, with an
// undecidable tier denied -- rather than to admitRowAuthzNode, which admits
// `granted`. The reason is admitRowAuthzBuiltinResult's own: admitRowAuthzNode
// may defer to a filter, because on that path a filter ran carrying the tier's
// spec as a top-level conjunct, so the join has already happened. Here nothing
// ran. A hand-rolled `bun.NewSelect()` carries no injected conjunct, no tier
// spec and no join, so there is nothing for an undecidable tier to defer to.
//
// A row with an EMPTY concept is admitted, matching every other seam. See
// admitRowAuthzBuiltinResult for why that is a decision rather than an
// oversight, and why changing it belongs in rowAuthzAdmits' undeclared branch
// where it would apply to every seam at once.
//
// # It is DELEGATION, not a second implementation
//
// One line, no state, no cache, no second lookup -- the same standing constraint
// ConceptDataIsStaged records next door, and for the same reason. Every defect
// this area has been filed for is two detectors that drifted (memql#2779,
// memql#3612, memql#2875), and the drift is reliably fail-open. A direct-SQL
// reader that computed "may this caller see this row" its own way would agree on
// the day it was written and diverge silently afterwards.
//
// # Reaching it
//
// Prefer the injected form. PluginContext.AdmitSourceRow carries this function
// to every plug-in, and a capability that takes it as a REQUIRED constructor
// parameter cannot be built without one -- the discipline recentChat already
// applies to its staged-data predicate. Packages below component/memql in the
// module graph (component/harness) cannot import this one at all and have no
// other option.
//
// # This does not make a hand-rolled read SCOPED
//
// Worth stating because the two are easy to conflate. The gate answers "may this
// caller see this ROW", per row, after the fetch. It does not add a caller
// predicate to the SQL, so a read whose only narrowing is a caller-supplied
// argument still fetches everything that argument names and then filters. That
// is a correctness fix, not a performance one, and it is not a substitute for
// routing the read through the authorized path -- which is the wider inventory
// memql#3984 is for.

import (
	"context"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// AdmitSourceRow reports whether one row read DIRECTLY from the node store may
// be shown to this caller.
//
// Call it on the rows as they come back, BEFORE folding, summarizing or
// repacking them -- see this file's header for why that is the only point where
// the answer is computable.
//
// ORDERING MATTERS WHEN THE CALLER DEDUPS. A reader that folds several versions
// of one id down to the latest must select the latest FIRST and gate that,
// rather than gating the raw rows and folding what survives: gating first lets a
// denied latest version fall through to an admitted older one, so the caller is
// handed a stale row instead of no row. That is the quietest failure in this
// class, because a plausible answer is returned rather than an empty one. It is
// the same ordering hazard withheldAsStaged records for the staged-data check
// beside it, arriving from the authorization side.
func AdmitSourceRow(ctx context.Context, node memorynodes.MemoryNode) bool {
	return admitRowAuthzBuiltinResult(ctx, node)
}

// AdmitSourceRows filters a slice of directly-read rows down to the ones this
// caller may see, leaving the slice untouched when none was denied.
//
// The convenience form for a reader with no per-id fold to worry about. A reader
// that DOES fold must use AdmitSourceRow inside the fold instead; see the
// ordering note there.
func AdmitSourceRows(ctx context.Context, nodes []memorynodes.MemoryNode) []memorynodes.MemoryNode {
	return filterRowAuthzBuiltinNodes(ctx, nodes)
}
