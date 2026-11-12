package memql

// authoring_promote_concept.go -- teaching a running cluster a new CONCEPT
// (memql#3746, the anchor of the construct-training epic memql#3745).
//
// A cluster could already be taught a query / mutation / logic / spec / trait at
// runtime -- persisted, live on every node in seconds, re-hydrated at boot, no
// restart. It could not be taught a concept, and that is the kind that matters
// most: a customer's domain arrives as NOUNS first, and every other construct
// kind binds to one.
//
// Why this is tractable at all, and why it needs no migration: every row lives
// in ONE generic hypertable -- MemoryNodes(id, "createdAt", "createdBy", schema,
// payload, metadata, "type", concept) -- so a new concept is a new STRING VALUE
// in the `concept` column. There is no per-concept table to create and no DDL to
// run. Concepts with no directory on disk are also an existing, sanctioned
// category: concept_ids.go already carries a block of runtime concepts
// "registered dynamically (no concept directory on disk)", and
// ValidateConceptConstants walks only AllFilesystemConcepts().
//
// What the promote actually has to get right is the DERIVED state. See
// promoteConceptIntoLiveRegistry for that; it is the whole substance of this
// file.
//
// Not here, and settled elsewhere: DEMOTE semantics, which landed as memql#3756
// in authoring_concept_retire.go -- a demote RETIRES a concept with rows under it
// (registered, readable, closed to new writes) and REMOVES one with none. Promote
// is that decision's un-retire path, which is the single line at the end of
// PromoteAuthoredConstruct's concept branch. Still open: the re-promote SCHEMA
// DIFF (memql#3757 -- additive lands, breaking is refused).

import (
	"context"
	"fmt"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// compileAuthoredConcept compiles a concept construct's source into the
// *memoryNodes.Concept the engine registers, mirroring the sandbox's Gate-1
// concept path exactly (it calls the same buildCandidateConcept). Stored as the
// construct's Compiled form so promotion can merge it into the live registry
// without re-parsing -- the same contract compileAuthoredFunction and
// compileAuthoredSpec hold for their kinds.
func compileAuthoredConcept(c SandboxConstruct) (*memoryNodes.Concept, error) {
	_, concept, err := buildCandidateConcept(c)
	if err != nil {
		return nil, err
	}
	return concept, nil
}

// conceptBindingRegistry returns the concept set a stored authored construct
// recompiles + binds against at re-hydration: an ISOLATED clone of this
// engine's live registry, so a construct bound to a PROMOTED concept resolves
// it (memql#3746) rather than only to the core set.
//
// A clone rather than the registry itself because the compile path is a
// consumer, not an owner, of that binding context -- the same posture the Gate-1
// sandbox takes. An engine whose registry is not a *MemoryRegistry (test stubs)
// falls back to a clone of the package default, which is what the caller got
// before this existed.
func (e *MemQLEngine) conceptBindingRegistry() memoryNodes.Registry {
	if registry, ok := e.concepts.(*memoryNodes.MemoryRegistry); ok {
		return registry.Clone()
	}
	return memoryNodes.CloneDefaultRegistry()
}

// promoteConceptIntoLiveRegistry merges an already-compiled candidate concept
// into the engine's LIVE concept registry and rebuilds everything the engine
// derives from that registry.
//
// # Merging is the easy half; the derivation is the point
//
// MemQLEngine.Init does not merely STORE the concept registry, it DERIVES state
// from it, and two of this issue's own invariants live inside that derivation:
// relationship normalization + validation (which produces e.relationships, what
// drives id canonicalization, (concept, id) resolution and traversal) and the
// node-type invariants on the `type` column. A concept merged into the registry
// AFTER Init is absent from all of it. Rows would write and read perfectly well
// while its @relationship edges silently did nothing and its node type was never
// checked -- a promoted `collection` with no `contains` edge, a promoted
// `@relationship` to a core concept that canonicalizes no id. So the derivation
// was factored out of Init (deriveConceptRegistryState) and this path calls the
// SAME function over the whole registry. Promotes are rare; re-deriving all of
// it is cheap, and one implementation is the only way the two paths cannot drift.
//
// # Order: derive first, merge second
//
// deriveConceptRegistryState runs over a CANDIDATE set -- the live registry's
// concepts plus this one -- before anything is merged. A candidate that fails
// any invariant therefore leaves the live registry exactly as it was, with no
// rollback to write. (MemoryRegistry.Remove exists since memql#3756, so a
// merge-then-validate ordering could now technically undo itself -- but it would
// still have published an invalid concept to every reader in between, and the
// derivation would have run over it. Derive-first has no such window, which is
// why the demote path mirrors this ordering rather than the reverse.) The
// derivation normalizes in place and is idempotent, so re-walking the
// already-derived core concepts costs nothing and changes nothing.
//
// # Which registry
//
// e.concepts is the concept.Registry INTERFACE. Production binds it to
// memoryNodes.DefaultRegistry() (app/database.go) and BuildOfflineSense binds it
// to a clone, so this mutates whatever registry THIS engine is bound to -- never
// the package-level memoryNodes.MergeAll, which would reach past the engine and
// hit the global. A registry that is not a *MemoryRegistry (test stubs) is
// REFUSED with a message naming the type, rather than silently mutating
// something else or silently doing nothing.
//
// # Core-first, never-shadow
//
// The same one-way guarantee PromoteAuthoredConstruct enforces for functions and
// specs, applied to the concept registry: a canonical id a CORE concept already
// owns is refused. A re-promote of a previously promoted id is allowed and
// replaces it -- the marker under "concept:<id>" is what tells the two apart.
//
// # What a re-promote may CHANGE (memql#3757)
//
// Not "whatever compiles". A re-promote whose schema differs from the version
// this cluster is running is classified: additive lands, breaking is refused
// naming the field + a real row count + the real referencing constructs, and an
// explicit override lands it anyway and is audited. The rules and the rendering
// live in authoring_concept_diff.go; this path holds only the hook, and holds it
// in the branch where a PRIOR version exists -- which is what makes a FIRST
// promote structurally unable to reach the classifier.
func (e *MemQLEngine) promoteConceptIntoLiveRegistry(ctx context.Context, gate *conceptPromoteGate, c *AuthoredConstruct, built *memoryNodes.Concept) error {
	id := strings.TrimSpace(built.Name)
	if id == "" {
		return fmt.Errorf("authoring: promote concept %q: compiled concept carries no canonical id", c.Name)
	}

	if e.concepts == nil {
		return fmt.Errorf("authoring: promote concept %q: concept registry is not initialized", c.Name)
	}
	registry, ok := e.concepts.(*memoryNodes.MemoryRegistry)
	if !ok {
		return fmt.Errorf(
			"authoring: promote concept %q: this engine's concept registry is a %T, not the mutable *memoryNodes.MemoryRegistry a promote merges into",
			c.Name, e.concepts)
	}

	key := "concept:" + id
	if existing, err := registry.Get(id); err == nil && existing != nil {
		if _, promoted := e.promotedAuthored.Load(key); !promoted {
			return fmt.Errorf("authoring: promote concept %q: a core concept already owns the id %q (promotion cannot redefine core)", c.Name, id)
		}
		// memql#3757: this is a RE-promote, and `existing` is the version this
		// cluster is currently running. Classify the difference BEFORE anything
		// is merged, persisted or broadcast -- a refusal must leave the live
		// registry untouched and must never reach a peer.
		if err := e.gateConceptSchemaChange(ctx, gate, c.OwnerUserId, existing, built); err != nil {
			return fmt.Errorf("authoring: promote concept %q: %w", c.Name, err)
		}
	}

	// The candidate set: every concept currently registered, with this one
	// added (or replacing its prior promotion). All() hands back a copy of the
	// map, so building the candidate set touches nothing live.
	current := registry.List()
	candidates := registry.All()
	candidates[id] = built
	list := make([]*memoryNodes.Concept, 0, len(candidates))
	for _, candidate := range candidates {
		list = append(list, candidate)
	}

	relationships, err := deriveConceptRegistryState(list)
	if err != nil {
		return fmt.Errorf("authoring: promote concept %q: %w", c.Name, err)
	}

	// Which account the concept's rows belong to (design record
	// docs/superpowers/specs/2026-11-12-account-export-design.md, E5). A concept
	// authored at runtime must declare it: nothing outside this repository can
	// have written one before this check, so unlike a mounted bundle it gets
	// no deprecation window. Checked after the derivation, whose refusals (a
	// relationship to nothing, a broken node type) are about the concept's
	// shape and come first, and before anything is merged.
	if built.BelongsTo == nil {
		return fmt.Errorf("authoring: promote concept %q: %w", c.Name,
			undeclaredBelongsToRefusal(id, "a concept taught to a running cluster declares one"))
	}

	// The cross-concept @belongsTo checks a load runs (belongsToProblems),
	// over the candidate set, after the derivation has resolved each
	// relationship's target, which is what a `via` is followed through. Only
	// a problem THIS promote introduces refuses it: one the live registry
	// already had would refuse every promote for a fault that is not the
	// author's, so the live set's problems are subtracted. Refused with the
	// load's own code and words.
	//
	// A REPLAY skips it. Boot re-hydration walks the stored bundles in the
	// order the store returns them, so a child reached before its parent would
	// be judged against a registry the parent is not in yet
	// (belongs_to_parent_unknown) and quarantined: a sound pair would load or
	// not by storage order. The boot walk judges every stored concept once
	// they are all in (checkRehydratedBelongsTo); a cross-node propagation
	// replays a promote its origin already judged.
	if gate == nil || !gate.replay {
		if err := newBelongsToProblem(current, list); err != nil {
			return fmt.Errorf("authoring: promote concept %q: %w", c.Name, err)
		}
	}

	registry.MergeAll(map[string]*memoryNodes.Concept{id: built})
	e.setConceptRelationships(relationships)

	// The schema index is the other thing Init derives from the registry: it is
	// what spec validation resolves a bound concept's fields through, so a
	// promoted concept with no entry would be invisible to every spec bound to
	// it. Rebuilt from the registry rather than patched, for the same
	// one-implementation reason.
	schemaIdx, err := buildSchemaIndex(registry)
	if err != nil {
		return fmt.Errorf("authoring: promote concept %q: rebuild schema index: %w", c.Name, err)
	}
	e.setSchemaIndex(schemaIdx)

	// Keyed by the CANONICAL ID, not the declaration name: that is the key the
	// concept registry itself uses, and the key the construct catalog looks a
	// concept up under (catalogConcepts reports c.Name, which is the id). A
	// marker filed under the declaration name would leave the catalog reporting
	// a promoted concept as `core`.
	e.promotedAuthored.Store(key, newPromotedMarker(c.Source))

	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("authored concept merged into the live concept registry",
			"component", "memql.engine", "concept", id, "name", c.Name, "owner", c.OwnerUserId)
	}

	// Registry-change delta (memql#4238). Fired here, at the shared-registry
	// merge, so BOTH the local promote and a peer node's re-hydration
	// (rehydratePromotedBundleNow, which funnels through this same function)
	// notify the clients connected to THAT node -- which is what makes a promote
	// on replica A visible to a client on replica B without a reconnect. An add
	// and a re-promote both ride Added; the client upserts by id.
	e.broadcastConceptAdded(built)
	return nil
}

// newBelongsToProblem returns the first @belongsTo cross-concept refusal the
// candidate set has and the current set did not, or nil.
func newBelongsToProblem(current, candidates []*memoryNodes.Concept) error {
	had := map[string]bool{}
	for _, p := range belongsToProblems(current) {
		had[p.concept+"\x00"+p.err.Error()] = true
	}
	for _, p := range belongsToProblems(candidates) {
		if !had[p.concept+"\x00"+p.err.Error()] {
			return p.err
		}
	}
	return nil
}

// checkRehydratedBelongsTo runs the load's cross-concept @belongsTo checks over
// the whole registry once the boot walk has put every stored concept back, the
// way checkRehydratedPushdown runs the predicate checks: the walk skipped them
// per promote (see promoteConceptIntoLiveRegistry) because a child can be
// reached before its parent. A PROMOTED concept a problem is filed under is
// taken back out of the registry and quarantined, then the checks run again,
// since removing a parent leaves its children with nothing to follow. A
// problem filed under a core concept is the load's to report, not this walk's,
// and is left alone. It returns the quarantined constructs, as kind:name.
func (e *MemQLEngine) checkRehydratedBelongsTo() []string {
	if e == nil {
		return nil
	}
	registry, ok := e.concepts.(*memoryNodes.MemoryRegistry)
	if !ok {
		return nil
	}
	var quarantined []string
	for {
		removed := false
		for _, p := range belongsToProblems(registry.List()) {
			key := "concept:" + p.concept
			if _, promoted := e.promotedAuthored.Load(key); !promoted {
				continue
			}
			if err := e.removeConceptFromLiveRegistry(registry, p.concept); err != nil {
				// Something else still references it; leaving it registered
				// is the safe outcome, and the problem is reported either way.
				e.quarantineRehydratedConstruct(AuthoringConstructRow{Kind: "concept", Name: p.concept}, fmt.Errorf("%w (and it could not be removed: %v)", p.err, err))
				quarantined = append(quarantined, "concept:"+p.concept)
				e.promotedAuthored.Delete(key)
				continue
			}
			e.promotedAuthored.Delete(key)
			e.quarantineRehydratedConstruct(AuthoringConstructRow{Kind: "concept", Name: p.concept}, p.err)
			quarantined = append(quarantined, "concept:"+p.concept)
			removed = true
		}
		if !removed {
			return quarantined
		}
	}
}
