package memql

// staged_transition.go -- the STAGED -> LIVE transition for a promoted concept's
// data, and its cross-node propagation (epic memql#3974, task memql#3986).
//
// memql#3980 built the tier's STATE: a durable `conceptDataStaged` flag on the
// v1:authoring:construct row, an in-memory marker under
// "conceptDataStaged:<canonicalId>", a promote-side intent
// (WithConceptDataStaged), and a boot replay that folds the rows back into the
// marker with "a live row wins". It deliberately built no way OUT. This file is
// that way out, and it is the only one.
//
// # The transition
//
//	TrainConceptData(owner, name)
//	  -> resolve the declaration name to the canonical id, through the SAME gate
//	     the demote path uses (so a sealed CORE concept is unreachable);
//	  -> clear `conceptDataStaged` on EVERY stamped row for that concept;
//	  -> clear the in-memory marker;
//	  -> broadcast on the EXISTING authoring.promote.<bundleId> channel.
//
// DURABLE HALF FIRST, MARKER SECOND -- the promote path's order, and the
// argument survives the reversal of direction. A crash between the two leaves
// the durable record saying LIVE and this one process still hiding: the rows are
// visible everywhere else, they are visible here after a restart, and an
// operator can see the disagreement. The other order leaves the durable record
// saying STAGED while this process shows the rows, which converges on HIDDEN --
// the direction memql#3980 records as indistinguishable from data loss.
//
// EVERY STAMPED ROW IS CLEARED, not just one. The fold would already resolve the
// concept live off a single cleared row, so this is not needed for correctness of
// the replay; it is needed because a row left stamped keeps asserting, on every
// review surface that reads it, that data is being withheld when it is not.
// setStagedRowsStatus writes every matching row for the neighbouring tier and
// this is the same reasoning arriving at the same place.
//
// # Why the transition is one-way, and what that cost
//
// There is no `stageConceptDataAgain`. Staging is a promote-time intent, and
// re-hiding rows a reader has already seen is not a thing an engine can offer
// honestly -- the disclosure has happened. A concept whose data must be withheld
// again is a concept whose rows must be deleted, which is a different operation
// with a different name.
//
// Making the transition one-way forced RULING (a) below, because until this file
// there were TWO doors out of staged and one of them was unmarked.
//
// # RULING (a): a re-promote CARRIES THE STAGING FORWARD (memql#3986)
//
// It did not, and that fell out of the fold rather than being chosen. A promote
// appends a construct row; an ordinary promote writes it unstamped; "a live row
// wins" resolves the concept LIVE. So an author iterating on an untrained
// concept's schema -- the loop the whole tier exists for -- published every row
// they had accumulated by fixing a typo.
//
// That is a DISCLOSURE caused by an operation whose name is about a SCHEMA, it is
// silent, and it cannot be undone. The mirror-image rule the retire path records
// is "data must never be made unreachable by an operation whose name suggests it
// affects only a definition" (authoring_concept_retire.go); this is that rule
// arriving from the other side, where the cost is worse because unreachable is
// recoverable and disclosed is not.
//
// It also made the transition a fiction. An operation is not one-way and
// deliberate if a second, unnamed operation performs it by accident.
//
// So: the promote path now asks conceptDataStageIntent, which is
// `requested || alreadyStaged`, and stamps the new row when either holds. THE
// FOLD IS UNCHANGED -- carry-forward is implemented by STAMPING, never by
// reweighting the fold, whose direction is settled and not up for revision.
//
// The alternative -- keep requiring the flag to be restated -- was rejected on
// the asymmetry of the two mistakes. Forgetting to restate publishes data
// permanently; restating unnecessarily withholds data that one named call
// releases.
//
// WHAT CARRY-FORWARD READS, AND THE WINDOW IT LEAVES. The source is the
// IN-MEMORY MARKER, not the durable rows, and that is forced rather than
// preferred: reading the rows would mean a bundle walk on every promote, and
// -- decisively -- that walk does not work in production today (see the
// memql#4036 note below), so it would answer "not staged" every time and
// carry nothing forward at all. The marker is the engine's own converged
// belief: every node computes it from the same durable rows at boot, and
// resolveConceptDataStagingFromStore below re-computes it on every promote
// broadcast. The residual window is a promote landing on a node that has not
// yet applied the broadcast; that node writes an unstamped row and the concept
// resolves live. It is the mesh's ordinary eventual-consistency window, it is
// bounded by the broadcast, and TestConceptDataStagedReplay_ALiveRowWins now
// builds its live row exactly that way, so the window is pinned rather than
// assumed away.
//
// # RULING (b): retirement and staging stay INDEPENDENT (memql#3986)
//
// memql#3980 gave the two facts separate key namespaces precisely so a concept
// could hold both. This is what that means in operation:
//
//	TRAINING A RETIRED CONCEPT IS ALLOWED, and changes nothing about the
//	retirement. The two answer different questions -- may rows be WRITTEN, may
//	rows be READ -- and the transition is a statement about the second only.
//	Refusing would make one axis a precondition of the other, and the concrete
//	case is the sympathetic one: an author stages a concept, accumulates rows,
//	then demotes it (rows exist, so it RETIRES). The rows are intact and hidden.
//	If training were refused, the only route to them would be a re-promote --
//	re-opening the concept to WRITES purely in order to READ data already held.
//	That is the retire path's own rule 1, from the other side.
//
//	RETIRING A STAGED CONCEPT changes nothing about the staging, with one
//	exception that is not an exception to the independence: the REMOVE outcome.
//	A demote with rows RETIRES and leaves the marker alone; a demote with ZERO
//	rows REMOVES the concept from the registry entirely, and there the marker is
//	cleared with the others, because it would otherwise name a concept nothing
//	can resolve. That branch has no data to decide the visibility of by
//	definition. Both halves live in authoring_concept_retire.go, next to the
//	code they qualify.
//
// # Cross-node: the EXISTING broadcast, and why no new topic
//
// The transition rides authoring.promote.<bundleId> -- publishAuthoringPromote,
// the channel the durable promote and the memql#3928 train already use, with the
// single broadcast routing rule already in component/node/routing.go.
//
// A dedicated authoring.stagedData.* topic would need its own routing rule, its
// own subscriber and its own delivery-order relationship with the promote
// channel it would race. And it would carry no information the promote channel
// does not: the payload is a bundle id, the receiving node reads the durable
// rows itself, and after this transition those rows ARE the answer. The tier's
// state is a field on a construct row inside a promote bundle; "re-read this
// bundle's constructs" is exactly the message that already exists.
//
// The receiving side is the piece this file adds. rehydratePromotedBundleNow
// re-REGISTERS a bundle's constructs and says nothing about their visibility, so
// before this a peer that received a staged promote registered the concept and
// believed its rows were live. resolveConceptDataStagingFromStore closes that:
// the same fold the boot replay runs, over the same row set, IN BOTH DIRECTIONS
// -- it marks what the rows say is staged and CLEARS what they say is live. The
// boot replay only marks, which is right for a fresh engine whose marker map is
// empty and wrong for a running one, where a transition is precisely a marker
// that must come off.
//
// THE FOLD IS GLOBAL, not scoped to the broadcast bundle, and it has to be. A
// concept accumulates one construct row per promote, each in its own bundle, so
// no single bundle holds the row set the concept's visibility is a function of.
// Scoping to the broadcast bundle would make a peer's answer depend on which
// bundle happened to be named -- and the cost of that being wrong is a cluster
// where the same row is visible or not depending on which replica answered.
// The walk is gated on the broadcast bundle actually carrying a concept row, so
// the ordinary promote of a query or a spec pays nothing.
//
// # NO BACKLOG REPLAY, and this was ruled in memql#3979
//
// The transition emits ONE concept-grain event. It does NOT emit the per-row
// node.created events that were withheld while the concept was staged, and
// nothing here reconstructs them.
//
// Emitting them would fire every subscribed automation once per row: 10,000 rows
// is 10,000 automation runs in a burst, for rows that arrived over an arbitrary
// period, each carrying a createdAt scattered across the past while arriving now.
// Any automation reasoning about recency -- which is most of them -- would be
// reasoning about a timeline that never happened. The visibility change is one
// fact about one concept, so it is one event about one concept.
//
// # memql#4036: THE DURABLE HALF IS UNREACHABLE IN PRODUCTION TODAY
//
// enginePromoteRehydrateStore.LoadPromotedBundles prefix-matches the
// durable-promote bundle prefix against node.GetId(), which over the in-process
// Execute path is the CANONICAL id (v1:authoring:bundle:mcp-promote-...), so it
// matches nothing and returns zero bundles. That is a pre-existing defect, older
// than this epic, filed as memql#4036 and deliberately NOT fixed here -- it
// changes boot behaviour well beyond this issue.
//
// Every path in this file that reads durable rows goes through that enumeration,
// so on a live cluster today: the transition's row walk finds nothing, the
// propagation resolve folds nothing, and memql#3980's boot replay -- which has
// the same dependency -- replays nothing. The tier is effectively
// single-process and non-durable until #4036 lands, and this file is correct on
// the day it does.
//
// What this file refuses to do is be SILENT about it. A transition that
// enumerates ZERO durable bundles while holding a promotion marker for the
// concept it was asked to train reports DurableRecordUnreachable and logs a
// warning naming #4036 -- because "trained, 0 rows cleared" and "the durable
// record is not readable from here" are the same sentence otherwise, and an
// operator running the transition is exactly the person who needs to be told
// which one happened.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// AuditActionConceptDataTrained is the audit action stamped when a promoted
// concept's staged DATA is made live. lower_snake_case, matching the
// v1:identity:auditEvent.action convention the promote / demote / stage / train
// actions use.
//
// Distinct from AuditActionConstructTrained (memql#3928), and the two can never
// describe the same event: that one is a CONSTRUCT leaving owner-scoped
// resolution for the shared registry, and its tier refuses `concept` by name,
// while this one is a CONCEPT's ROWS leaving the staged read tier. Sharing an
// action name would collapse two different subjects into one audit line.
const AuditActionConceptDataTrained = "concept_data_trained"

// ConceptDataTrainOutcome is the structured result of one staged -> live
// transition: what it applied to, whether anything changed, and the evidence.
//
// Structured for the reason DemoteOutcome is: the client renders it, and a
// transition that reports only "ok" leaves the operator unable to tell "the rows
// are now live" from "they already were" from "this node cannot see the durable
// record at all". Those three are the whole decision space here and they need
// different next steps.
type ConceptDataTrainOutcome struct {
	// Name is the concept as the CALLER named it -- a declaration name or a
	// canonical id, whichever they passed.
	Name string `json:"name"`
	// ConceptId is the canonical id the name resolved to.
	ConceptId string `json:"conceptId"`
	// Trained reports whether THIS call changed the durable record. False is a
	// successful, idempotent no-op: the concept's data was already live (a
	// second call, or a concept nobody staged). It is not a failure and does not
	// come with an error -- a transition that could only be run once and errored
	// afterwards could not be safely retried after a partial write.
	Trained bool `json:"trained"`
	// RowsCleared is how many stamped v1:authoring:construct rows were written.
	// Reported alongside Trained for DemoteOutcome.RowCount's reason: it is the
	// evidence, and "trained, 3 rows" is checkable where "trained" is not.
	RowsCleared int `json:"rowsCleared"`
	// DurableRecordUnreachable reports that the walk enumerated ZERO
	// durable-promote bundles while the concept is author-promoted -- which
	// cannot both be true, and today means memql#4036 (see this file's header).
	// The transition still clears the in-memory marker and still reports success;
	// this is what stops "0 rows cleared" from reading as "nothing needed
	// clearing".
	DurableRecordUnreachable bool `json:"durableRecordUnreachable,omitempty"`
}

// --- the transition ----------------------------------------------------------

// TrainConceptData makes a promoted concept's STAGED rows live: they stop being
// withheld from the ordinary read path and join it, in place, with no row
// rewritten (epic memql#3974, task memql#3986). One-way -- see this file's
// header.
//
// owner is the AUTHENTICATED actor. The gate is the CALLER's, exactly as it is
// for promote and demote (the owner-only check lives on the gRPC handler, not in
// the engine); what the engine enforces here is that the named concept is
// AUTHOR-PROMOTED, which is what makes a sealed core concept unreachable from
// this path -- a core concept carries no promotion marker, so it can never have
// been staged and can never be trained.
//
// name may be the declaration name (`trainedWidget`) or the canonical id
// (`v1:trainingns:trainedWidget`); an ambiguous declaration name is refused
// naming both candidates rather than resolved to one.
//
// It is IDEMPOTENT. A concept whose data is already live returns
// Trained=false with no error, so a retry after a partial durable write is safe
// and so is a caller that does not know the current state.
func (e *MemQLEngine) TrainConceptData(ctx context.Context, owner, name string) (ConceptDataTrainOutcome, error) {
	return e.trainConceptDataWithStore(ctx, &engineConceptDataStore{engine: e}, owner, name)
}

// trainConceptDataWithStore is the store-driven core, split out so it is unit
// testable with a fake conceptDataStore (no live DB) -- the shape every durable
// authoring path in this package takes.
func (e *MemQLEngine) trainConceptDataWithStore(ctx context.Context, store conceptDataStore, owner, name string) (ConceptDataTrainOutcome, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" {
		return ConceptDataTrainOutcome{}, fmt.Errorf("authoring: training a concept's staged data requires an authenticated owner")
	}
	if name == "" {
		return ConceptDataTrainOutcome{}, fmt.Errorf("authoring: training a concept's staged data requires a concept name")
	}

	// The gate AND the identity resolution in one call, shared verbatim with the
	// demote path (authoring_concept_retire.go). A name that is not
	// author-promoted is refused here, before any row is read or written.
	conceptId, err := e.resolvePromotedConceptId("train", name)
	if err != nil {
		return ConceptDataTrainOutcome{}, err
	}
	outcome := ConceptDataTrainOutcome{Name: name, ConceptId: conceptId}

	// The rows are named by the DECLARATION name even when the caller addressed
	// the concept by canonical id -- the two identities again, resolved the way
	// the demote walk resolves them.
	declName := conceptDeclarationName(conceptId)

	bundles, err := store.LoadPromotedBundles(ctx)
	if err != nil {
		return ConceptDataTrainOutcome{}, fmt.Errorf("authoring: enumerate promoted bundles to train concept %q: %w", conceptId, err)
	}
	// See the memql#4036 note in this file's header. Zero bundles for a concept
	// the engine holds a promotion marker for is a contradiction, not a state.
	outcome.DurableRecordUnreachable = len(bundles) == 0

	broadcastBundleId, broadcastOwner := "", ""
	for _, bundle := range bundles {
		rows, lerr := store.LoadConstructsForBundle(ctx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return outcome, fmt.Errorf("authoring: load constructs to train concept %q: %w", conceptId, lerr)
		}
		for _, row := range rows {
			if !conceptDataRowIsStagedFor(row, declName) {
				continue
			}
			// Written under the ROW's own owner, not the caller's. Visibility is
			// a pure function of the CONCEPT (memql#3977), and a concept is
			// shared cluster-wide -- so a transition that stopped at the caller's
			// own bundles could leave a stamped row behind under another owner,
			// and the fold would resolve the concept STAGED again at the next
			// boot while the operator had been told it was trained. The demote
			// path walks every owner's bundles for exactly this reason and writes
			// each row under its bundle's owner; this is that walk.
			rowOwner := strings.TrimSpace(row.OwnerUserId)
			if rowOwner == "" {
				rowOwner = bundle.OwnerUserId
			}
			if terr := store.TrainConstructConceptData(ctx, rowOwner, row.Id); terr != nil {
				// Return the outcome ALONGSIDE the error: rows written before
				// this one stay written, and a caller that can see how far it
				// got can retry (the transition is idempotent) instead of
				// guessing.
				return outcome, fmt.Errorf("authoring: clear the staged-data stamp on construct row %q: %w", row.Id, terr)
			}
			outcome.RowsCleared++
			broadcastBundleId, broadcastOwner = bundle.Id, bundle.OwnerUserId
		}
	}

	// THE MARKER COMES OFF EITHER WAY, including when no row was cleared. Two
	// states reach here with RowsCleared == 0 and both want the marker gone: the
	// concept's data is already live durably (so a marker still set is stale and
	// hides rows the durable record says are visible), or the durable record is
	// unreachable (memql#4036, reported above), where the epic's asymmetry says
	// to fail toward VISIBLE -- disclosure an operator can see and act on, rather
	// than a withholding indistinguishable from data loss.
	e.clearConceptDataStaging(conceptId)
	outcome.Trained = outcome.RowsCleared > 0

	if e.Component != nil && e.Logger != nil {
		switch {
		case outcome.DurableRecordUnreachable:
			e.Logger.Warn("trained a concept's staged data with NO durable record reachable from this node: the marker is cleared here and nothing was written, so no peer and no restart will learn of it (memql#4036 -- the durable-promote bundle enumeration matches no rows)",
				"component", "memql.engine", "concept", conceptId, "name", name, "owner", owner,
				"action", AuditActionConceptDataTrained)
		case outcome.Trained:
			e.Logger.Info("promoted concept's staged data is now LIVE: the rows written under it while it was staged join the ordinary read path, in place",
				"component", "memql.engine", "concept", conceptId, "name", name, "owner", owner,
				"rowsCleared", outcome.RowsCleared, "action", AuditActionConceptDataTrained)
		default:
			e.Logger.Info("concept's data was already live; training it changed nothing",
				"component", "memql.engine", "concept", conceptId, "name", name, "owner", owner,
				"action", AuditActionConceptDataTrained)
		}
	}

	// ONE broadcast, on the EXISTING channel, naming ONE of the bundles whose
	// rows changed -- and which one does not matter, because the receiving node's
	// resolve folds over EVERY promoted bundle rather than the one it was handed.
	// What the bundle id has to be is a bundle that carries a concept row, which
	// every bundle this loop touched does by construction; that is what gets the
	// receiver past its cheap gate and into the fold.
	//
	// The owner in the payload is the BUNDLE's, not the caller's: the receiver
	// uses it to read that bundle's constructs under the right envelope, and a
	// caller who is not the bundle owner would name an envelope those rows are
	// not visible in.
	if outcome.Trained {
		e.publishAuthoringPromote(broadcastBundleId, broadcastOwner)
	}
	return outcome, nil
}

// conceptDataRowIsStagedFor reports whether a persisted construct row is a
// stamped, still-live row for the named concept -- the row set the transition
// writes.
//
// The status filter is the boot replay's, verbatim (authoring_concept_staged.go:
// applyPersistedConceptDataStaging), and it has to be: this function decides
// which rows a transition CLEARS and that fold decides which rows a restart
// READS, so a row one of them counts and the other does not is a concept that
// comes back staged after being trained. A status-retired row never re-registers
// its concept; a status-staged row belongs to the memql#3928 construct tier,
// which a concept row can never be in.
func conceptDataRowIsStagedFor(row AuthoringConstructRow, declName string) bool {
	if row.Kind != "concept" || row.Name != declName {
		return false
	}
	if isRetiredConstructStatus(row.Status) || isStagedConstructStatus(row.Status) {
		return false
	}
	return row.ConceptDataStaged
}

// --- the promote-side carry-forward (RULING (a)) -------------------------------

// conceptDataStageIntent resolves whether THIS promote should land its concept
// with the data staged: the caller asked for it, OR the concept's data is
// already staged and a promote must not publish it (see RULING (a) in this
// file's header).
//
// It derives the canonical id the same way applyConceptDataStagingOnPromote does
// a moment later, off the compiled form rather than the declaration name, and
// the duplication is deliberate rather than shared through a helper there:
// memql#3980's file states its own tier and this is the ruling that came after
// it, so the decision that changed is visible in the file that changed.
//
// A non-concept kind is never staged and never carries anything forward -- it
// owns no rows.
func (e *MemQLEngine) conceptDataStageIntent(c *AuthoredConstruct, requested bool) bool {
	if requested {
		return true
	}
	if e == nil || c == nil || c.Kind != "concept" || c.Compiled == nil {
		return false
	}
	built, ok := c.Compiled.(*memoryNodes.Concept)
	if !ok || built == nil {
		return false
	}
	return e.conceptDataIsStaged(built.Name)
}

// --- the cross-node resolve ----------------------------------------------------

// resolveConceptDataStagingAfterBroadcast brings this node's staged-data markers
// into agreement with the durable rows after an authoring.promote broadcast --
// the receiving half of the cross-node propagation.
//
// It runs only when the broadcast bundle actually carries a CONCEPT row, so the
// ordinary promote of a query, a mutation or a spec pays a single already-loaded
// bundle read and nothing else. The gate is on the bundle rather than on the
// event payload because the payload carries a bundle id and an owner, and
// nothing about which kinds are inside it.
//
// Failures are LOGGED, never propagated. The caller is a broadcast subscriber
// whose other job -- re-registering the bundle's constructs -- has already
// succeeded by the time this runs, and there is nobody to return an error to.
// The direction of the degrade is the epic's: a resolve that does not run leaves
// this node showing rows another node withholds, which an operator can see and
// correct with one more transition, where the opposite leaves rows withheld with
// nothing to say they are.
func (e *MemQLEngine) resolveConceptDataStagingAfterBroadcast(ctx context.Context, store promoteRehydrateStore, owner, bundleId string) {
	if e == nil || store == nil {
		return
	}
	rows, err := store.LoadConstructsForBundle(ctx, owner, bundleId)
	if err != nil {
		if e.Component != nil && e.Logger != nil {
			e.Logger.Warn("authoring promote propagation: could not read the broadcast bundle to resolve staged-data visibility; this node's view of which concepts' rows are withheld may disagree with its peers until the next boot",
				"component", "memql.engine", "bundleId", bundleId, "owner", owner, "error", err)
		}
		return
	}
	carriesConcept := false
	for _, row := range rows {
		if row.Kind == "concept" {
			carriesConcept = true
			break
		}
	}
	if !carriesConcept {
		return
	}
	staged, live, err := e.resolveConceptDataStagingFromStore(ctx, store)
	if err != nil {
		if e.Component != nil && e.Logger != nil {
			e.Logger.Warn("authoring promote propagation: staged-data resolve failed; this node's view of which concepts' rows are withheld may disagree with its peers until the next boot",
				"component", "memql.engine", "bundleId", bundleId, "owner", owner, "error", err)
		}
		return
	}
	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("authoring promote propagation: staged-data visibility resolved from the shared durable rows",
			"component", "memql.engine", "bundleId", bundleId, "owner", owner,
			"conceptsStaged", staged, "conceptsMadeLive", live)
	}
}

// resolveConceptDataStagingFromStore folds the persisted construct rows into this
// engine's staged-data markers IN BOTH DIRECTIONS, and returns (marked, cleared).
//
// It is memql#3980's applyPersistedConceptDataStaging over the same row set with
// the same fold -- concept-grain, "a live row wins" -- plus the half a boot
// replay does not need. That replay runs on a fresh engine whose marker map is
// empty, so marking is the whole job. This runs on a RUNNING engine, where a
// transition is exactly a marker that has to come OFF, and a resolve that only
// marked would leave every peer hiding rows the cluster had already published.
//
// It touches only concept ids it actually saw. A concept whose bundle could not
// be read is left alone rather than resolved as live, because "no evidence" and
// "evidence of live" are different and only one of them should move a marker.
//
// A per-bundle load failure fails the whole resolve rather than being skipped,
// and that is the opposite of the boot walk's quarantine posture for a good
// reason: the boot walk is deciding whether a construct REGISTERS, where a
// skipped row costs one construct, while this is deciding what is VISIBLE, where
// a skipped row costs a concept resolved from a partial row set -- which is how
// a live row goes unseen and a trained concept goes back into hiding.
func (e *MemQLEngine) resolveConceptDataStagingFromStore(ctx context.Context, store promoteRehydrateStore) (marked, cleared int, err error) {
	bundles, err := store.LoadPromotedBundles(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("authoring: enumerate promoted bundles for staged-data resolve: %w", err)
	}

	type tally struct{ staged, live bool }
	byConceptId := make(map[string]*tally)

	for _, bundle := range bundles {
		rows, lerr := store.LoadConstructsForBundle(ctx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return 0, 0, fmt.Errorf("authoring: load constructs for staged-data resolve: %w", lerr)
		}
		for _, row := range rows {
			if row.Kind != "concept" || isRetiredConstructStatus(row.Status) || isStagedConstructStatus(row.Status) {
				continue
			}
			concept, cerr := compileAuthoredConcept(SandboxConstruct{Name: row.Name, Kind: row.Kind, Source: row.Source})
			if cerr != nil || concept == nil || strings.TrimSpace(concept.Name) == "" {
				// The walk's quarantine posture, and here it is also the honest
				// answer: a row whose source no longer compiles did not
				// re-register its concept either, so it names nothing whose
				// visibility this could decide.
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

	for conceptId, t := range byConceptId {
		// A LIVE ROW WINS -- spelled exactly as memql#3980's replay spells it, so
		// an inversion in either is visible as a difference between the two.
		if !t.staged || t.live {
			if e.conceptDataIsStaged(conceptId) {
				e.clearConceptDataStaging(conceptId)
				cleared++
			}
			continue
		}
		if !e.conceptDataIsStaged(conceptId) {
			marked++
		}
		e.markConceptDataStaged(conceptId)
	}
	return marked, cleared, nil
}

// --- the store -----------------------------------------------------------------

// conceptDataStore is the narrow graph surface the staged -> live transition
// needs: enumerate the durable-promote bundles, load one bundle's constructs,
// and clear a construct row's staged-data stamp.
//
// It is not the demoteStore and not the stagedRowStore. demoteStore's writers
// hard-code their own terminal states, and stagedRowStore's SetConstructStatus
// writes `status` -- which this transition must never touch, because a status
// the boot walk skips leaves the concept unregistered and its rows unreadable
// rather than live. Reusing either would put the wrong column one argument away.
// *MemQLEngine satisfies it via engineConceptDataStore; tests fake it.
type conceptDataStore interface {
	LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error)
	LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
	// TrainConstructConceptData clears `conceptDataStaged` on a
	// v1:authoring:construct row and leaves `status` alone -- the exact inverse
	// of promoteStore.StageConstructConceptData, and separate from it for the
	// same reason the stamp is separate from the status: one says which tier the
	// CONSTRUCT is in, the other whether the concept's ROWS are visible.
	TrainConstructConceptData(ctx context.Context, owner, constructId string) error
}

// engineConceptDataStore is the production conceptDataStore over a live engine.
// The enumeration + per-bundle load are the promote-rehydrate store's, verbatim,
// so the transition sees exactly the rows the boot fold will read; the clear runs
// the #954 lifecycle mutation under the row owner's envelope like every other
// authoring write in this package.
type engineConceptDataStore struct {
	engine *MemQLEngine
}

func (s *engineConceptDataStore) LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error) {
	return (&enginePromoteRehydrateStore{engine: s.engine}).LoadPromotedBundles(ctx)
}

func (s *engineConceptDataStore) LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	return (&enginePromoteRehydrateStore{engine: s.engine}).LoadConstructsForBundle(ctx, owner, bundleId)
}

// TrainConstructConceptData clears the concept-only staged-DATA flag through the
// dedicated trainConstructConceptData mutation, under the row owner's envelope --
// the #954 mutation reads actor.userId for the per-row authz owner, so a clear
// under any other actor simply matches no row and reports success having written
// nothing.
//
// NAMED ARGS, not the `name({...})` object-literal wrapper: that form was REMOVED
// from the grammar (memql#2335) and the parser refuses it, so a call written that
// way fails on a live engine while every fake-store unit test passes. That is not
// hypothetical here -- it is how the neighbouring persist calls stayed broken for
// months, and it is why this file ships with a db-gated test that executes this
// exact string.
func (s *engineConceptDataStore) TrainConstructConceptData(ctx context.Context, owner, constructId string) error {
	authorCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	_, err := s.engine.Execute(authorCtx,
		fmt.Sprintf("trainConstructConceptData(constructId: %s)", languageParser.QuoteString(constructId)))
	return err
}
