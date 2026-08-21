package memql

// authoring_concept_retire.go -- what it MEANS to withdraw a promoted concept
// (memql#3756, design §7.2; sibling of the promote anchor memql#3746).
//
// Withdrawing a function makes it uncallable and that is the whole story. A
// concept is different in one respect that decides everything else here: rows
// were written under it, and those rows outlive the definition. Unregistering
// the concept does not delete them -- it makes them UNREADABLE, because every
// read path resolves a concept id through the registry, so the rows sit in the
// hypertable addressed by a name the engine no longer knows.
//
// # The decision
//
//	rows under the concept | outcome
//	-----------------------+-----------------------------------------------
//	any                    | RETIRED -- stays in the registry marked retired,
//	                       | name stays claimed, existing rows stay readable,
//	                       | NEW WRITES REFUSED, re-promoting un-retires it
//	zero                   | REMOVED -- registry entry gone, name free again
//
// Two rules, recorded so they are not re-litigated:
//
//  1. Data must never be made unreachable by an operation whose name suggests it
//     affects only a definition. "Demote the concept" is a statement about a
//     schema; a reader losing access to rows they wrote is not something that
//     sentence can be read as authorising.
//  2. A concept promoted by TYPO, with nothing ever written to it, must still be
//     cleanly withdrawable -- otherwise `wodget` is taken forever on that
//     cluster, and the only repair is a redeploy.
//
// The two rules pull in opposite directions and the row count is the only thing
// that can arbitrate between them, which is why the count is not an optimisation
// here but the whole mechanism.
//
// # Who counts the rows
//
// countConceptRows reads STORAGE, not the query path, under no actor at all.
// That is the point rather than a shortcut: the per-row authz model filters
// reads by the caller (an owned-tier concept's rows are the caller's rows), so a
// count taken under the demoting owner would answer "how many rows can I see",
// and the outcome of the demote would depend on WHO ASKED. An owner who wrote
// nothing but whose colleague wrote a thousand rows would get "zero" and REMOVE
// the definition -- rule 1 violated by the very check meant to enforce it. There
// is also no cluster-owner DSL query to borrow: a promoted concept has no query
// construct bound to it, and inventing one per demote would be a second place
// for the count to be wrong. So the count goes to the table the rows are in.
//
// # Where the retired state lives
//
// In e.promotedAuthored, under the reserved "conceptRetired:<canonicalId>" key
// namespace (no registry kind is named `conceptRetired`, so it can collide with
// nothing). Not a new engine field, because the retirement is the same class of
// fact as the promotion marker beside it -- per-engine, keyed by canonical id,
// consulted by the same lifecycle -- and the two are coupled at EVERY
// transition: a promote clears the retirement and sets the marker, a
// retire-demote sets the retirement and KEEPS the marker (the concept is still
// author-promoted; that is what lets a later demote find it), a remove-demote
// clears both. Two maps with that coupling is two chances for one to be updated
// and the other not.
//
// # Durability, and why there is no un-retire write
//
// The persisted half is a `conceptRetired` flag on the v1:authoring:construct
// row (NOT status: "retired", which is the REMOVE outcome's marker and makes the
// boot walk skip the row entirely). Re-hydration registers the concept from the
// row's source exactly as before and then applyPersistedConceptRetirements folds
// the rows back into the retired set, so a restart comes back retired.
//
// The fold resolves per canonical id with "a LIVE row wins": a re-promote writes
// a NEW construct row whose conceptRetired is false, and that un-stamped row is
// what un-retires the concept across a restart -- no un-retire write, and
// nothing to get wrong about the ORDER two rows for one concept are walked in.
// The asymmetry is deliberate in that direction: a stale stamp resolving to
// "live" costs writes the owner can refuse again with one more demote, while a
// stale stamp resolving to "retired" silently refuses writes to a concept its
// owner has already re-promoted, which is indistinguishable from the engine
// being broken.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// DemoteOutcomeRetired / DemoteOutcomeRemoved are the two outcomes of a demote,
// as reported on DemoteOutcome.Outcome and over the wire
// (DurableDemoteOutcome.outcome). Lower-case, matching the persisted-status
// vocabulary the rest of the authoring lifecycle uses.
const (
	// DemoteOutcomeRetired: the construct is still REGISTERED and its name is
	// still claimed. Only a concept with rows under it reaches this.
	DemoteOutcomeRetired = "retired"
	// DemoteOutcomeRemoved: the construct is gone from the shared registry and
	// its name is claimable again. Every non-concept kind reaches this, and so
	// does a concept with zero rows.
	DemoteOutcomeRemoved = "removed"
)

// DemoteOutcome is the structured, per-construct result of a demote: WHICH of
// the two things happened, and the evidence that chose it.
//
// Structured rather than prose because the client renders it. A demote that
// reports only "ok" leaves the operator to infer from the absence of an error
// whether the name they just withdrew is free again -- which is the one thing
// they need to know next, and the one thing a success message cannot be read
// backwards to establish.
type DemoteOutcome struct {
	// Kind + Name are the construct as the SOURCE named it (for a concept,
	// Name is the declaration name, not the canonical id).
	Kind string `json:"kind"`
	Name string `json:"name"`
	// ConceptId is the canonical concept id the outcome applies to. Empty for
	// every non-concept kind -- they have no second identity.
	ConceptId string `json:"conceptId,omitempty"`
	// Outcome is DemoteOutcomeRetired or DemoteOutcomeRemoved.
	Outcome string `json:"outcome"`
	// RowCount is the number of DISTINCT rows found under the concept, counted
	// across every owner (see this file's header). It is what CHOSE the
	// outcome, so it is reported alongside it rather than left to be inferred:
	// "retired, 412 rows" and "removed, 0 rows" are the two sentences an
	// operator needs, and the second is only trustworthy if the first is
	// possible. Always 0 for a non-concept kind, which has no rows.
	RowCount int64 `json:"rowCount"`
}

// conceptRetiredKey is the e.promotedAuthored key namespace for the retired
// state. "conceptRetired:" cannot collide with the promotion markers
// ("function:" / "spec:" / "concept:") because no registry is named that.
func conceptRetiredKey(conceptId string) string { return "conceptRetired:" + conceptId }

// conceptIsRetired reports whether a promoted concept has been demoted while
// rows existed under it -- registered, readable, closed to new writes. Read on
// the mutation write path (executeWrite), so it is a single sync.Map load
// against a key built by one concatenation.
func (e *MemQLEngine) conceptIsRetired(conceptId string) bool {
	if e == nil || strings.TrimSpace(conceptId) == "" {
		return false
	}
	_, retired := e.promotedAuthored.Load(conceptRetiredKey(conceptId))
	return retired
}

// markConceptRetired closes a promoted concept to new writes while leaving it
// registered. Idempotent.
func (e *MemQLEngine) markConceptRetired(conceptId string) {
	e.promotedAuthored.Store(conceptRetiredKey(conceptId), promotedMarker{})
}

// clearConceptRetirement re-opens a retired concept to writes. Called from the
// concept branch of PromoteAuthoredConstruct -- a re-promote IS the un-retire
// path, and it is the only one, so there is no separate API to forget to call
// and no state a promote can leave half-lifted. A no-op for a concept that was
// never retired, which is every ordinary promote.
func (e *MemQLEngine) clearConceptRetirement(conceptId string) {
	if e == nil || strings.TrimSpace(conceptId) == "" {
		return
	}
	e.promotedAuthored.Delete(conceptRetiredKey(conceptId))
}

// errConceptWriteRetired names the retirement on a refused write. The message is
// the whole point of this being its own error rather than a schema failure: an
// operator whose insert stops working needs to learn that the DEFINITION was
// withdrawn and that a re-promote lifts it -- a generic "validation failed" or
// "concept not found" sends them to look at their payload.
func errConceptWriteRetired(conceptId string) error {
	return fmt.Errorf(
		"concept %q is RETIRED: it was demoted while rows already existed under it, so the rows stay readable and new writes are refused (memql#3756). Re-promote the concept to resume writing to it",
		conceptId)
}

// conceptRowCounter counts the rows stored under a canonical concept id. The
// production implementation is MemQLEngine.countConceptRows; it is a seam (the
// engine's conceptRowCount field) so the retire/remove DECISION is testable
// without a live database, which is the half of this file that is pure logic.
type conceptRowCounter func(ctx context.Context, conceptId string) (int64, error)

// conceptRowCounter resolves the counter this engine decides with: the injected
// seam when a test set one, the production storage count otherwise.
func (e *MemQLEngine) conceptRowCounter() conceptRowCounter {
	if e.conceptRowCount != nil {
		return e.conceptRowCount
	}
	return e.countConceptRows
}

// countConceptRows counts the DISTINCT rows stored under a concept, across every
// owner and every tier -- see this file's header for why the count deliberately
// runs under no actor.
//
// DISTINCT id, not raw row count: MemQL is append-only, so one row updated twice
// is three physical versions of ONE row. Reporting "3" for a single node would
// make the number the operator is shown disagree with the number they can see,
// and the number is being shown precisely so they can check the decision.
//
// SecretMemoryNodes is not consulted: no write path routes a promoted concept's
// rows there (it backs the separate SecretConcept registry, which authoring
// cannot reach), so counting it would be counting a table this decision can
// never have rows in.
// staged-data: MUST-NOT-GATE -- this count decides RETIRE vs REMOVE, so gating
// it REMOVES a concept that holds ten thousand rows (epic memql#3974, task
// memql#3984).
//
// The file header above already settles this in its own words and this tier
// does not get an exception from it: the count "reads STORAGE, not the query
// path, under no actor at all", because "a count taken under the demoting owner
// would answer 'how many rows can I see', and the outcome of the demote would
// depend on WHO ASKED". A staged predicate is that same scoping under a
// different name -- it answers "how many rows are currently visible" for a
// question that has to be "how many rows exist".
//
// Consequence in full: staged rows count as zero, zero routes to REMOVE, the
// registry entry goes and the name is freed, and rule 1 above -- data must
// never be made unreachable by an operation whose name suggests it affects only
// a definition -- is violated by the very check written to enforce it.
func (e *MemQLEngine) countConceptRows(ctx context.Context, conceptId string) (int64, error) {
	db := e.database()
	if db == nil {
		// FAIL CLOSED. With no database there is no evidence either way, and
		// the two outcomes are not symmetric: guessing "retired" costs a name
		// that stays claimed, guessing "removed" costs the readability of every
		// row that may exist. Refuse and say so.
		return 0, fmt.Errorf(
			"authoring: cannot count the rows under concept %q (no database on this node), and a concept demote must not choose between retire and remove without them", conceptId)
	}
	var count int64
	err := db.NewSelect().
		Model((*memoryNodes.MemoryNode)(nil)).
		ColumnExpr("count(distinct id)").
		Where("concept = ?", conceptId).
		Scan(ctx, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("authoring: count rows under concept %q: %w", conceptId, err)
	}
	return count, nil
}

// demoteConceptFromLiveRegistry is the concept branch of the in-process demote:
// it resolves the declaration name to a canonical id, counts the rows, and
// applies whichever of the two outcomes the count selects.
//
// It carries the same SAFETY GATE the other kinds do and for the same reason: a
// concept with no promotion marker is REFUSED, so a sealed core concept -- which
// a promotion can never shadow, and which therefore never carries a marker --
// can never be unregistered by a demote. The refusal keeps the exact
// "not an author-promoted construct" wording the other branches use, because the
// cross-node demote subscriber treats that substring as its idempotency signal
// (a node that never handled the original promote has nothing to remove).
func (e *MemQLEngine) demoteConceptFromLiveRegistry(ctx context.Context, name string) (DemoteOutcome, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return DemoteOutcome{}, fmt.Errorf("authoring: demote requires a construct name")
	}
	if e.concepts == nil {
		return DemoteOutcome{}, fmt.Errorf("authoring: demote concept %q: concept registry is not initialized", name)
	}
	registry, ok := e.concepts.(*memoryNodes.MemoryRegistry)
	if !ok {
		return DemoteOutcome{}, fmt.Errorf(
			"authoring: demote concept %q: this engine's concept registry is a %T, not the mutable *memoryNodes.MemoryRegistry a demote removes from",
			name, e.concepts)
	}

	conceptId, err := e.resolvePromotedConceptId("demote", name)
	if err != nil {
		return DemoteOutcome{}, err
	}

	rows, err := e.conceptRowCounter()(ctx, conceptId)
	if err != nil {
		return DemoteOutcome{}, err
	}

	outcome := DemoteOutcome{Kind: "concept", Name: name, ConceptId: conceptId, RowCount: rows}

	if rows > 0 {
		// RETIRE. Everything stays: the registry entry (so reads resolve), the
		// promotion marker (so the concept is still recognisably author-owned,
		// and a later demote can still find it). Only writes stop.
		//
		// THE STAGED-DATA MARKER STAYS TOO, UNTOUCHED (epic memql#3974, ruled in
		// memql#3986). Retirement and staging answer different questions -- may
		// rows be WRITTEN, may rows be READ -- and a concept in both states is a
		// coherent thing: rows accumulated while it was staged, and the
		// definition since withdrawn. Clearing the staging here would publish
		// them, which is the opposite of what withdrawing a definition can mean;
		// deepening it would be equally arbitrary. The train transition
		// (TrainConceptData) is what publishes them, and it works on a retired
		// concept precisely so an author is never forced to re-open a concept to
		// writes in order to read data they already hold.
		e.markConceptRetired(conceptId)
		outcome.Outcome = DemoteOutcomeRetired
		if e.Component != nil && e.Logger != nil {
			e.Logger.Info("authored concept retired: still registered and readable, new writes refused",
				"component", "memql.engine", "concept", conceptId, "name", name, "rows", rows)
		}
		return outcome, nil
	}

	// REMOVE. Nothing was ever written under it, so there is no data to strand
	// and the name goes back to being claimable.
	if err := e.removeConceptFromLiveRegistry(registry, conceptId); err != nil {
		return DemoteOutcome{}, err
	}
	e.promotedAuthored.Delete("concept:" + conceptId)
	e.clearConceptRetirement(conceptId)
	// AND the staged-data marker (epic memql#3974, ruled in memql#3986). This is
	// the branch where the two states stop being independent, and only this one:
	// the concept is now UNREGISTERED, so a marker keyed to its canonical id
	// names something nothing can resolve. Every other marker this branch holds
	// is deleted for that reason and this one is no different -- a read seam
	// consulting a marker for an absent concept is misled by state that outlived
	// its subject, which is exactly what memql#3980's replay declines to create
	// when it skips status-retired rows. Reaching here with a marker set requires
	// a staged concept with ZERO rows under it, which is the concept nobody ever
	// wrote to; there is no data whose visibility this can decide.
	e.clearConceptDataStaging(conceptId)
	outcome.Outcome = DemoteOutcomeRemoved
	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("authored concept removed from the live concept registry (no rows were ever written under it)",
			"component", "memql.engine", "concept", conceptId, "name", name)
	}
	return outcome, nil
}

// removeConceptFromLiveRegistry deletes a concept from the live registry and
// rebuilds everything the engine derives from it -- the mirror image of
// promoteConceptIntoLiveRegistry, and it has to be, for the same reason: the
// engine DERIVES relationship state and the schema index from the registry, so a
// concept dropped from the map while its derived state remains would leave
// relationship edges and a schema entry pointing at a concept nothing can
// resolve.
//
// DERIVE FIRST, REMOVE SECOND, exactly like the promote. Deriving over the
// candidate set (everything except this concept) BEFORE mutating means a removal
// that another concept's relationship depends on fails with the live registry
// untouched, rather than half-applied with no way back.
func (e *MemQLEngine) removeConceptFromLiveRegistry(registry *memoryNodes.MemoryRegistry, conceptId string) error {
	remaining := registry.All()
	if _, ok := remaining[conceptId]; !ok {
		// Not registered here. Not an error: the cross-node demote subscriber
		// runs this on every node, including ones that never registered it.
		return nil
	}
	delete(remaining, conceptId)

	list := make([]*memoryNodes.Concept, 0, len(remaining))
	for _, candidate := range remaining {
		list = append(list, candidate)
	}
	relationships, err := deriveConceptRegistryState(list)
	if err != nil {
		return fmt.Errorf("authoring: demote concept %q: the remaining concepts do not derive without it (something still references it): %w", conceptId, err)
	}

	registry.Remove(conceptId)
	e.setConceptRelationships(relationships)

	schemaIdx, err := buildSchemaIndex(registry)
	if err != nil {
		return fmt.Errorf("authoring: demote concept %q: rebuild schema index: %w", conceptId, err)
	}
	e.setSchemaIndex(schemaIdx)

	// Registry-change delta (memql#4238). Only the REMOVE branch of a demote
	// changes the registry SET, so only it broadcasts -- a retire leaves the
	// concept registered and readable, so ConceptsListMsg still returns it and
	// there is nothing for a set-shaped delta to say. Fired here (not in the
	// early not-registered return) so a node that never held the concept, which
	// the cross-node demote subscriber also runs this on, emits nothing.
	e.broadcastConceptRemoved(conceptId)
	return nil
}

// resolvePromotedConceptId maps what a CALLER has -- the declaration name out of
// the bundle source, or off a persisted construct row -- onto the canonical id
// the registry and the promotion marker are keyed by.
//
// The two identities are the whole difficulty. `concept trainedWidget` in a
// source registers as `v1:trainingns:trainedWidget`; a demote arrives naming
// `trainedWidget` and would find nothing under that key. The id is derivable
// from the SOURCE, but a demote deliberately never compiles (that is what lets a
// construct whose source has rotted still be withdrawn), so it is resolved from
// what the engine already holds instead.
//
// A canonical id passed straight in resolves as itself, so a caller that has one
// never has to take it apart.
//
// An AMBIGUOUS declaration name -- two promoted concepts in different namespaces
// whose ids end in the same segment -- is refused naming both ids, rather than
// picking one. Demoting the wrong concept is the single most expensive mistake
// this path can make.
//
// op NAMES THE OPERATION IN THE REFUSAL, and it is a parameter rather than the
// hard-coded "demote" it was because a second caller arrived: the staged-data
// transition (memql#3986, staged_transition.go) needs exactly this resolution
// AND exactly this gate -- the "not an author-promoted construct" refusal is what
// makes a sealed CORE concept unreachable from either path, since a core concept
// carries no promotion marker. Telling an operator who ran a train that their
// "demote" was refused would send them to look at the wrong operation.
//
// The refusal's WORDING after the operation name is unchanged and must stay so:
// the cross-node demote subscriber treats "not an author-promoted construct" as
// its idempotency signal.
func (e *MemQLEngine) resolvePromotedConceptId(op, name string) (string, error) {
	notPromoted := fmt.Errorf(
		"authoring: %s concept %q: not an author-promoted construct (demotion cannot remove a core construct)", op, name)

	if _, promoted := e.promotedAuthored.Load("concept:" + name); promoted {
		return name, nil
	}
	if e.concepts == nil {
		return "", notPromoted
	}

	var matches []string
	for _, c := range e.concepts.List() {
		if c == nil || conceptDeclarationName(c.Name) != name {
			continue
		}
		if _, promoted := e.promotedAuthored.Load("concept:" + c.Name); !promoted {
			continue
		}
		matches = append(matches, c.Name)
	}
	switch len(matches) {
	case 0:
		return "", notPromoted
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf(
			"authoring: %s concept %q: the name is ambiguous -- %s are both promoted and both end in %q; address it by canonical id",
			op, name, strings.Join(matches, " and "), name)
	}
}

// conceptDeclarationName returns the declaration name inside a canonical concept
// id: the final colon-separated segment of `v<major>:<namespace>:<name>` (the
// namespace may itself be colon-scoped, which is why this takes the LAST segment
// rather than the third).
func conceptDeclarationName(conceptId string) string {
	if idx := strings.LastIndex(conceptId, ":"); idx >= 0 {
		return conceptId[idx+1:]
	}
	return conceptId
}

// applyPersistedConceptRetirements replays the retired state after a restart. It
// runs AFTER the re-hydration walk has registered the concepts, because it is
// only meaningful for a concept that is registered: retirement is "loaded, and
// closed to writes".
//
// The fold is over CANONICAL IDS rather than rows, and its rule is that a live
// row wins (see this file's header). Two rows for one concept is the normal
// shape after a demote-then-re-promote -- the demote stamps the rows that exist,
// the re-promote adds a fresh unstamped one -- and resolving them per-row while
// walking would make the outcome depend on which bundle the walk reached first.
//
// Rows the walk itself skips are skipped here identically: a status-retired row
// is the REMOVE outcome's marker, and a removed concept is not registered at all,
// so it has no retirement to replay.
//
// Returns the number of concepts marked retired. A store failure is returned;
// per-row failures are skipped, matching the walk's quarantine posture -- a
// stored row whose source no longer compiles was already not re-registered, so
// there is nothing to retire.
func (e *MemQLEngine) applyPersistedConceptRetirements(ctx context.Context, store promoteRehydrateStore) (int, error) {
	bundles, err := store.LoadPromotedBundles(ctx)
	if err != nil {
		return 0, fmt.Errorf("authoring: enumerate promoted bundles for concept-retirement replay: %w", err)
	}

	type tally struct{ retired, live bool }
	byConceptId := make(map[string]*tally)

	for _, bundle := range bundles {
		rows, lerr := store.LoadConstructsForBundle(ctx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return 0, fmt.Errorf("authoring: load constructs for concept-retirement replay: %w", lerr)
		}
		for _, row := range rows {
			if row.Kind != "concept" || isRetiredConstructStatus(row.Status) {
				continue
			}
			concept, cerr := compileAuthoredConcept(SandboxConstruct{Name: row.Name, Kind: row.Kind, Source: row.Source})
			if cerr != nil || concept == nil || strings.TrimSpace(concept.Name) == "" {
				continue
			}
			t := byConceptId[concept.Name]
			if t == nil {
				t = &tally{}
				byConceptId[concept.Name] = t
			}
			if row.ConceptRetired {
				t.retired = true
			} else {
				t.live = true
			}
		}
	}

	retired := 0
	for conceptId, t := range byConceptId {
		if !t.retired || t.live {
			continue
		}
		e.markConceptRetired(conceptId)
		retired++
		if e.Component != nil && e.Logger != nil {
			e.Logger.Info("promoted concept came back RETIRED at re-hydration (registered and readable, new writes refused)",
				"component", "memql.engine", "concept", conceptId)
		}
	}
	return retired, nil
}
