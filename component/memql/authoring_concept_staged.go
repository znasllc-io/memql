package memql

// authoring_concept_staged.go -- the STAGED DATA tier (epic memql#3974, task
// memql#3980): a promoted concept whose ROWS are not yet visible.
//
// THE TIER IS INERT IN THIS FILE'S FIRST FORM, AND THAT IS DELIBERATE. Nothing
// here filters a read, refuses a write, or suppresses an event. What lands is
// the STATE and the machinery that carries it -- the durable flag, the in-memory
// marker, the promote-side write, the boot replay -- so the seams that will
// consume it have something true to consume. Read enforcement is memql#3983, the
// write chokepoint is memql#3985, the staged -> live transition is memql#3986.
// Adding any of them here would mean shipping a visibility rule before the state
// it reads has ever been observed to survive a restart.
//
// # Why a sibling boolean, and not a `status` value
//
// The obvious move is to extend v1:authoring:construct.status. It is wrong
// TWICE, and the two reasons are independent -- either alone settles it.
//
//  1. THE NAME IS ALREADY TAKEN, BY SOMETHING ELSE. Epic memql#3928 landed
//     `staged` on that enum while this epic was queued, and its staged is a
//     property of the CONSTRUCT'S RESOLUTION SCOPE: the construct resolves for
//     its author alone and is not broadcast (authoring_staged.go). That tier
//     refuses `concept` BY NAME rather than skipping it, because a concept has
//     no owner-scoped resolution path at all -- it registers into the one shared
//     concept registry, and merging it rebuilds relationship + node-type state
//     cluster-wide, so a staged concept would persist a row and register an
//     entry NOTHING would ever read. This epic's staged is the opposite shape:
//     the concept IS registered shared, IS resolvable, and DOES accept writes.
//     Only the VISIBILITY OF ITS ROWS is withheld. Two facts that share a word
//     and disagree about the subject cannot share a column.
//  2. A STATUS VALUE WOULD MAKE THE ROWS UNREADABLE RATHER THAN STAGED. This is
//     memql#3756's argument, arriving unchanged at the same answer (see
//     authoring_concept_retire.go, which this file is the sibling of, field for
//     field). The boot walk routes on status; a value it skips leaves the
//     concept unregistered; a concept that is not registered cannot be resolved;
//     and rows addressed by a name the engine no longer knows are not hidden,
//     they are LOST. Staging data must never be one restart away from losing it.
//
// # Where the state lives, and why it is concept-grain
//
// Durably: a `conceptDataStaged` flag on the v1:authoring:construct row, beside
// `conceptRetired`. In memory: e.promotedAuthored, under the reserved
// "conceptDataStaged:<canonicalId>" key namespace, for the same reason the
// retirement lives there -- it is the same CLASS of fact as the promotion marker
// beside it (per-engine, keyed by canonical id, consulted by the same lifecycle),
// and the two are coupled at every transition, so two maps would be two chances
// for one to be updated and the other not.
//
// THERE IS NO ROW MARKER ON MemoryNodes AT ALL, and that is a measured ruling
// rather than a simplification. Against a real TimescaleDB container with every
// chunk compressed:
//
//	concept-grain (this)      | 1 row insert, 11ms, decompresses nothing
//	row-marked (UPDATE)       | 116-142ms per 10k rows, and a HARD ERROR under
//	                          | timescaledb.enable_dml_decompression=false
//	                          | ("UPDATE/DELETE is disabled on compressed chunks")
//	row-marked (append-only)  | no decompression, but doubles version history and
//	                          | makes every later read of that concept 3-5x
//	                          | slower PERMANENTLY (29-36ms vs 7-12ms)
//
// So visibility is a pure function of the CONCEPT. That is why the flag lives on
// the construct row and nowhere else, why the transition unit is the concept,
// and why the transition is one-way.
//
// # The default is FALSE, and that is load-bearing
//
// Every construct row that already exists carries no marker, and every ordinary
// promote writes none. A default that read as "staged" would hide every row in
// the installation the moment this shipped -- the exact failure `isNotDeleted`
// already made once, and the one this epic's brief names first among its risks.
//
// # The fold: A LIVE ROW WINS
//
// applyPersistedConceptDataStaging resolves per CANONICAL ID, not per row, and
// any row saying the data is live makes it live. Two rows for one concept is the
// normal shape here (each promote of the same source appends one), so resolving
// per-row while walking would make the outcome depend on which bundle the walk
// reached first.
//
// The asymmetry points this way ON PURPOSE, and it is not the direction that
// merely loses less -- it is the only direction whose failure is VISIBLE. A
// stale stamp resolving to LIVE discloses rows the owner can withhold again with
// one more transition, and they can SEE that it happened. A stale stamp
// resolving to STAGED hides the rows of a concept its owner has already trained:
// the data is intact, addressable and completely absent from every read, which
// is indistinguishable from having lost it and is discoverable only by someone
// who already suspects it.

import (
	"context"
	"fmt"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptDataStagedKey is the e.promotedAuthored key namespace for the staged
// DATA state. "conceptDataStaged:" collides with nothing: no registry kind is
// named that, and it is distinct from "conceptRetired:" beside it -- a concept
// can be both (retired to writes, its rows still staged), so the two markers
// must be separately addressable rather than two values of one key.
func conceptDataStagedKey(conceptId string) string { return "conceptDataStaged:" + conceptId }

// conceptDataIsStaged reports whether the rows written under a promoted concept
// are STAGED -- present and addressable, withheld from the ordinary read path
// until the concept is trained.
//
// INERT AS OF memql#3980: nothing consults it yet. It is a single sync.Map load
// against a key built by one concatenation because of where memql#3983 will
// consult it from -- the read seams, per plan -- and a predicate that has to be
// cheap after it is wired is cheaper to write cheap now.
func (e *MemQLEngine) conceptDataIsStaged(conceptId string) bool {
	if e == nil || strings.TrimSpace(conceptId) == "" {
		return false
	}
	_, staged := e.promotedAuthored.Load(conceptDataStagedKey(conceptId))
	return staged
}

// markConceptDataStaged withholds a promoted concept's rows from the ordinary
// read path while leaving the concept registered, resolvable and open to writes.
// Idempotent.
func (e *MemQLEngine) markConceptDataStaged(conceptId string) {
	if e == nil || strings.TrimSpace(conceptId) == "" {
		return
	}
	e.promotedAuthored.Store(conceptDataStagedKey(conceptId), promotedMarker{})
}

// clearConceptDataStaging makes a staged concept's rows live.
//
// Called from the durable promote path for EVERY concept that promotes without
// the staging intent, which is the ordinary promote and therefore almost all of
// them. That is not defensive tidying: it is what makes the in-process answer
// agree with the one the boot fold would compute over the same rows. An ordinary
// re-promote appends an UNSTAMPED row, "a live row wins" resolves that concept
// as live at the next boot, and an engine that left a stale marker in memory
// would hide rows until a restart un-hid them -- a divergence between two nodes
// of one cluster that no operator could see the cause of.
//
// A no-op for a concept whose data was never staged.
func (e *MemQLEngine) clearConceptDataStaging(conceptId string) {
	if e == nil || strings.TrimSpace(conceptId) == "" {
		return
	}
	e.promotedAuthored.Delete(conceptDataStagedKey(conceptId))
}

// --- the promote-side intent -------------------------------------------------

// PromoteDurableOption is an optional instruction to the durable promote path.
// EXPORTED because the callers that will pass one are outside this package (the
// gRPC durable-promote handler, memql#3987), while the config it writes into
// stays unexported so the set of instructions remains this package's to define.
//
// Variadic rather than a fourth positional bool, and that is a review decision
// worth recording: `allowBreaking` is already a positional bool on the same
// call, a second one beside it would make every existing call site read
// `..., false, false)` with nothing to say which is which, and adding it
// positionally would have churned sixteen call sites that have no opinion about
// this tier. `promoteBundleDurableWithStore(..., WithConceptDataStaged())` says
// at the call site what it does.
type PromoteDurableOption func(*promoteDurableConfig)

// promoteDurableConfig is the resolved instruction set for one durable promote.
// Its zero value is the ORDINARY promote -- shared, live, visible -- so a caller
// that passes nothing gets exactly the behaviour that predates this tier.
type promoteDurableConfig struct {
	// stageConceptData lands every CONCEPT in this promote with its rows staged.
	// It says nothing about the non-concept kinds in the same bundle: they have
	// no rows of their own, so there is nothing about them to withhold.
	stageConceptData bool
}

// WithConceptDataStaged promotes a concept with its DATA staged (epic
// memql#3974): the concept registers shared and accepts writes exactly as an
// ordinary promote does, and the rows written under it stay withheld from the
// ordinary read path until it is trained.
func WithConceptDataStaged() PromoteDurableOption {
	return func(c *promoteDurableConfig) { c.stageConceptData = true }
}

// newPromoteDurableConfig folds the options. No validation and no refusal: every
// option here is additive and independent, unlike the re-hydration config next
// door, which refuses a walk missing a branch of its three-way route because a
// missing branch there silently mis-routes a row.
func newPromoteDurableConfig(opts ...PromoteDurableOption) promoteDurableConfig {
	var cfg promoteDurableConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// applyConceptDataStagingOnPromote resolves ONE promoted construct's data
// visibility, durable half first.
//
// It is a no-op for every kind but `concept`: a query, a mutation, a spec and a
// trait have no rows of their own, so a bundle promoted under
// WithConceptDataStaged() stages its concepts and leaves its verbs untouched.
// That is not a silent drop -- there is nothing about a verb to withhold.
//
// PERSIST FIRST, MARK SECOND, and the order is the fail-safe one. The durable
// row is what the boot fold reads, so it is the source of truth and the marker
// is derived from it; a persist that fails therefore returns an error with
// NOTHING hidden anywhere, rather than leaving this process hiding rows that the
// next restart would reveal. The construct is already registered by the time we
// get here (the shared registration is the authoritative gate, above), so this
// error carries the same meaning every other persist failure on this path
// carries: promoted, not yet durable.
func (e *MemQLEngine) applyConceptDataStagingOnPromote(ctx context.Context, store promoteStore, c *AuthoredConstruct, constructId string, stage bool) error {
	if e == nil || c == nil || c.Kind != "concept" {
		return nil
	}
	// The CANONICAL id, off the compiled form -- not c.Name, which is the
	// DECLARATION name. The two identities are the same trap the retire path
	// records: `concept trainedWidget` registers as `v1:<ns>:trainedWidget`, and
	// a marker keyed by the short name would be looked up by nothing.
	built, ok := c.Compiled.(*memoryNodes.Concept)
	if !ok || built == nil || strings.TrimSpace(built.Name) == "" {
		return nil
	}
	conceptId := built.Name

	if !stage {
		// THE ORDINARY PROMOTE. See clearConceptDataStaging for why this runs
		// unconditionally rather than only when a marker is known to exist.
		e.clearConceptDataStaging(conceptId)
		return nil
	}

	if err := store.StageConstructConceptData(ctx, constructId); err != nil {
		return fmt.Errorf("authoring: stamp staged-data concept row: %w", err)
	}
	e.markConceptDataStaged(conceptId)
	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("promoted concept landed with its DATA STAGED (registered, resolvable and open to writes; its rows are withheld from the ordinary read path until it is trained)",
			"component", "memql.engine", "concept", conceptId, "name", c.Name, "constructId", constructId)
	}
	return nil
}

// --- the boot replay ----------------------------------------------------------

// applyPersistedConceptDataStaging replays the staged-DATA state after a
// restart. It runs AFTER the re-hydration walk has registered the concepts, for
// the same reason its retirement sibling does: the state is only meaningful for
// a concept that is registered. "Loaded, resolvable, and its rows withheld" has
// no meaning for a concept nothing can resolve.
//
// The fold is over CANONICAL IDS rather than rows, and A LIVE ROW WINS (see this
// file's header for why that direction and not the other). Two rows for one
// concept is the normal shape after a re-promote -- the staged promote stamps
// its row, a later ordinary promote adds a fresh unstamped one -- and resolving
// them per-row while walking would make the outcome depend on which bundle the
// walk reached first.
//
// Rows the walk itself skips are skipped here identically. A status-retired row
// never re-registered its concept, so it has no visibility to replay. A
// status-STAGED row cannot occur on a concept at all: that tier (memql#3928)
// refuses `concept` by name, and a row that somehow carried it would have been
// routed to the owner-scoped staged registry rather than the shared one -- so it
// is skipped rather than folded, which keeps this function honest about
// resolving only concepts that are actually in the shared registry.
//
// Returns the number of concepts whose data came back staged. A store failure is
// returned; per-row failures are skipped, matching the walk's quarantine posture
// -- a stored row whose source no longer compiles was already not re-registered,
// so there is nothing to stage.
func (e *MemQLEngine) applyPersistedConceptDataStaging(ctx context.Context, store promoteRehydrateStore) (int, error) {
	bundles, err := store.LoadPromotedBundles(ctx)
	if err != nil {
		return 0, fmt.Errorf("authoring: enumerate promoted bundles for concept staged-data replay: %w", err)
	}

	type tally struct{ staged, live bool }
	byConceptId := make(map[string]*tally)

	for _, bundle := range bundles {
		rows, lerr := store.LoadConstructsForBundle(ctx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return 0, fmt.Errorf("authoring: load constructs for concept staged-data replay: %w", lerr)
		}
		for _, row := range rows {
			if row.Kind != "concept" || isRetiredConstructStatus(row.Status) || isStagedConstructStatus(row.Status) {
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
			if row.ConceptDataStaged {
				t.staged = true
			} else {
				t.live = true
			}
		}
	}

	staged := 0
	for conceptId, t := range byConceptId {
		// A LIVE ROW WINS. Written as `!t.staged || t.live` rather than
		// `t.staged && !t.live` so the guard reads in the same direction the
		// retirement sibling's does; inverting either half is the mistake this
		// fold exists to prevent, and the two being spelled alike is what makes
		// an inversion visible in review.
		if !t.staged || t.live {
			continue
		}
		e.markConceptDataStaged(conceptId)
		staged++
		if e.Component != nil && e.Logger != nil {
			e.Logger.Info("promoted concept came back with its DATA STAGED at re-hydration (registered and writable; its rows are withheld from the ordinary read path until it is trained)",
				"component", "memql.engine", "concept", conceptId)
		}
	}
	return staged, nil
}
