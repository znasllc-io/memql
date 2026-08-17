package memql

// staged_scope.go -- THE EXPLICITLY STAGED-SCOPED READ (epic memql#3974, task
// memql#4040).
//
// memql#3976 ruled that a staged row is visible to nobody on the ordinary read
// path AND that it stays reachable "only through an explicitly staged-scoped
// read". memql#3983 shipped the first half. This is the second.
//
// # The gap it closes
//
// Until now a staged row was reachable by NOTHING. Rows could be written against
// a staged concept, they persisted, they survived a restart, and training made
// them live -- but between those two moments nothing could show an operator what
// was queued. Training is one-way and makes rows live to everyone at once, so
// the operator made that decision blind, and debugging a bad ingest meant
// reading "MemoryNodes" by hand.
//
// # Why #3983 built no scope, and why that was right
//
// enforceStagedDataOnPlan is injected from parseWithFunctionsAmbient, which
// takes no context.Context, and #3976 ruled explicitly that enforceRowAuthzOnPlan
// must not gain one. So a scope could not be read off the caller's context AT
// THE INJECTION SITE, which is where the conjunct is built. #3983 built NO scope
// rather than a partial one, on the argument that a half-wired scope the
// injection silently defeats is worse than none.
//
// # What that reasoning missed, and it was in the same function signature
//
// "The injection site has no ctx" was never the real constraint. The constraint
// is that the parse path is ctx-FREE BY DESIGN -- and this codebase already has
// a pattern for handing it a value that lives on the context, used TWICE, in the
// signature of the very function that does the injecting:
//
//	origin  auth.CallOrigin  -- memql#2800: "the origin is read ONCE here, from
//	                            the context, and handed to the (ctx-free) parse
//	                            path"
//	ambient map[string]any   -- memql#3024: "read ONCE here too, by the same
//	                            route and for the same reason as the origin"
//
// The staged scope rides exactly that way: executeWith resolves it once and
// hands it down as a parameter. The parse path stays ctx-free, no injector gains
// a context, and #3976's ruling is untouched.
//
// # The three decisions memql#4040 asked for
//
// ## 1. How a caller declares the intent
//
// EXPLICITLY, on the request context, via ContextWithStagedScope, naming the
// concept ids it wants staged rows for. Never inferred from identity -- that is
// what #3976 ruled against, and the reason is that an identity-derived predicate
// stops being a constant and drags a context into the injection seam.
//
// A context value is not an identity inference: it is a caller-set declaration
// that happens to travel on the request, which is the same thing auth.CallOrigin
// already is. It names CONCEPT IDS rather than being a "show me everything
// staged" boolean, so the request says what it wants and the authorization
// below has something per-concept to be about.
//
// GO-LEVEL, DELIBERATELY, because that is exactly as far as the write side
// reaches. WithConceptDataStaged has zero non-test callers and nothing on the
// wire stages a concept today (see authoring_concept_staged.go, which says so at
// length). A wire surface for reading staged rows, with no wire surface for
// creating them, would be a mechanism whose halves disagree about who can use
// it. When staging gets a wire surface, this gets one in the same change.
//
// ## 2. Who may use it
//
// THE CLUSTER OWNER. And the point memql#4040 insisted on is that this makes the
// AUTHORIZATION identity-derived while the PREDICATE stays constant -- so the
// two are separated deliberately rather than by accident:
//
//	AUTHORIZATION  identity-derived, resolved ONCE per read, at the seam where
//	               the context and the actor both exist (stagedScopeFor).
//	PREDICATE      a pure function of the RESOLVED scope. By the time the
//	               injection site sees it, it is a constant. It consults no
//	               identity, and it could not: it is built at plan time.
//
// WHY CLUSTER OWNER AND NOT THE CONCEPT'S OWNER, which memql#4040 calls the
// natural answer. The owner is not available where the decision has to be made.
// The in-memory staged marker is a bare promotedMarker{} under
// "conceptDataStaged:<id>" and carries no owner; resolving one means reading the
// authoring bundle row, i.e. a database round trip inside a gate that
// authoring_concept_staged.go requires to stay a single allocation-free sync.Map
// load because it runs on every read and every write. Cluster-owner is decidable
// from the actor envelope alone.
//
// It is also the right ANSWER and not merely the cheap one, because it matches
// what the capability is FOR: memql#4040's motivation is that "nothing can show
// an operator what is queued" and "the operator makes that decision blind". This
// is an operator inspection capability. If per-concept owner scoping is wanted
// later, this is the function to change, and it should carry the store lookup's
// cost explicitly rather than hiding it in the row gate.
//
// A DECLARED-BUT-UNAUTHORIZED SCOPE REFUSES THE READ rather than silently
// downgrading to an ordinary one. The caller asked for something specific; an
// empty result would not tell them whether nothing is staged or they were not
// allowed to look, and this epic's standing posture is that a component which
// cannot answer a visibility question must not answer it optimistically.
//
// ## 3. Whether the row gate honours it too
//
// YES, AND IT HAS TO. memql#4040: "a scope honoured by one and not the other
// yields a read that returns some staged rows and not others, which is worse
// than either answer." Both seams resolve through stagedConceptWithheld, the
// single function that decides whether a concept is withheld from THIS read:
//
//	the conjunct  stagedConceptIds -> stagedVisibilityPredicate  (plan time)
//	the row gate  admitStagedRow                                 (per row)
//
// The two reach the scope by different routes -- handed down as a parameter for
// the ctx-free parse path, read from the context for the row gate, which has one
// at both of its call sites -- but through ONE resolver, stagedScopeFor, so they
// cannot disagree about what was authorized.
//
// AND THE DIRECTION OF THE PUSHDOWN IS STILL RIGHT. staged_enforce.go's header
// warns that "a pushdown that hides rows the gate would admit is a pushdown that
// is wrong". The conjunct excludes staged-MINUS-scoped, the gate withholds
// staged-MINUS-scoped, so the conjunct never hides a row the gate admits. It
// remains an optimization over a correctness mechanism, which is the invariant
// that whole file is built on.
//
// ## A THIRD SURFACE IS DELIBERATELY LEFT UNSCOPED
//
// ConceptDataIsStaged -- the exported predicate the DIRECT-SQL readers take as
// an injected func (integrations/chat, component/harness) -- takes a concept id
// and no context, so it cannot see a scope and does not get one.
//
// Called out rather than left to be discovered, because memql#4040's own warning
// is that a scope honoured by one seam and not another is worse than either
// answer. That warning is about the two seams of a single read -- the conjunct
// and the row gate -- which agree here by construction. This is a different
// read, and the asymmetry runs the SAFE way: an unscoped predicate withholds
// MORE than the caller was authorized to see, never less.
//
// It is also the right answer on the merits. Those readers are capability
// surfaces, not inspection ones: recentChat's five reads exist to assemble LLM
// context. Widening them under an operator's scope would push staged rows into a
// model prompt as a side effect of an operator looking at a queue, which is a
// worse outcome than the operator having to read the rows through Execute. If a
// direct reader ever needs the scope, give THAT reader a ctx-taking predicate --
// do not make the shared one ctx-aware, or every capability inherits the
// widening by default.
//
// # The cache-key term is NOT optional (and this is the trap)
//
// The result cache keys off planCacheSignature, and staged_enforce.go's header
// records that "the conjunct is part of plan.Root, so a staged and an unstaged
// read of the same query cannot share a cache entry". A SCOPE breaks that
// argument in the one case that matters. When the scope covers every staged
// concept, the predicate is EMPTY -- there is nothing left to exclude -- so
// plan.Root is byte-identical to the plan an ordinary caller produces when
// nothing is staged at all.
//
// Without a cache term the sequence is: the operator reads with a scope, gets
// staged rows, the result is cached under that signature; an ordinary caller
// issues the same query, hits the cache, and is served the staged rows. The
// engine's gates all ran correctly and the rows were published anyway.
//
// So the resolved scope is a signature input, on exactly the reasoning
// planIsUnbound is one (memql#3350): the row gate makes the result
// caller-dependent, so the key must be too. It is added only when the scope is
// non-empty, which is every read on every installation that is not actively
// inspecting staged data, so every existing signature is byte-identical.

import (
	"context"
	"sort"
	"strings"
)

// stagedScopeKey is the context key carrying a caller's declared staged scope.
// Unexported type, so nothing outside this package can collide with it or forge
// one without going through ContextWithStagedScope.
type stagedScopeKey struct{}

// StagedScope is the set of concepts whose STAGED rows a read may see.
//
// The zero value is the ordinary read -- staged rows visible to nobody -- so
// every path that does not deliberately construct one gets #3983's behaviour
// unchanged. That default direction is the same one authoring_concept_staged.go
// calls load-bearing for the staged flag itself.
type StagedScope struct {
	// conceptIds is nil for the zero value. Never mutated after construction:
	// a resolved scope is handed to the parse path, read per row by the gate,
	// and folded into a cache key, so it has to be safe to share.
	conceptIds map[string]struct{}
}

// IsEmpty reports whether this scope widens nothing.
func (s StagedScope) IsEmpty() bool { return len(s.conceptIds) == 0 }

// includes reports whether conceptId's staged rows are in scope for this read.
func (s StagedScope) includes(conceptId string) bool {
	if len(s.conceptIds) == 0 {
		return false
	}
	_, ok := s.conceptIds[strings.TrimSpace(conceptId)]
	return ok
}

// signature renders the scope as a stable cache-key fragment: the sorted concept
// ids, comma-joined. Empty for the zero value, which is what keeps every
// ordinary read's cache key byte-identical to what it was before this file.
func (s StagedScope) signature() string {
	if len(s.conceptIds) == 0 {
		return ""
	}
	ids := make([]string, 0, len(s.conceptIds))
	for id := range s.conceptIds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// ContextWithStagedScope declares an explicitly staged-scoped read: rows of the
// named concepts are visible to THIS read even though their data is staged.
//
// The declaration is only half of it. Whether it is HONOURED is decided by
// stagedScopeFor, which requires the caller to be the cluster owner; declaring a
// scope one may not use makes Execute refuse rather than quietly return an
// ordinary read. Stamping a context is therefore not a privilege escalation, and
// could not be: the value carries no identity and grants nothing by itself.
//
// Blank ids are dropped. Calling with none returns ctx unchanged, so
// `ContextWithStagedScope(ctx, ids...)` with an empty slice is not accidentally
// a request for something.
func ContextWithStagedScope(ctx context.Context, conceptIds ...string) context.Context {
	declared := make(map[string]struct{}, len(conceptIds))
	for _, id := range conceptIds {
		if id = strings.TrimSpace(id); id != "" {
			declared[id] = struct{}{}
		}
	}
	if len(declared) == 0 {
		return ctx
	}
	return context.WithValue(ctx, stagedScopeKey{}, StagedScope{conceptIds: declared})
}

// stagedScopeDeclaredOn returns the scope a caller asked for, before
// authorization. Callers other than the two functions below want stagedScopeFor.
func stagedScopeDeclaredOn(ctx context.Context) StagedScope {
	if ctx == nil {
		return StagedScope{}
	}
	scope, _ := ctx.Value(stagedScopeKey{}).(StagedScope)
	return scope
}

// stagedScopeFor resolves the scope this read actually gets: the caller's
// declaration INTERSECTED with their authorization to make it.
//
// THE SINGLE RESOLVER, and every seam calls it rather than reading the context
// itself. That is what makes the mechanism safe without any cooperation from the
// entry point: a code path that parses and executes without passing through
// executeWith's refusal below still gets an EMPTY scope here unless the caller
// is the cluster owner, because the authorization is part of resolving the value
// rather than a check somebody has to remember to perform first.
//
// An unauthorized declaration resolves to the zero scope -- fail-CLOSED -- and
// executeWith separately turns that case into an explicit error, so the caller
// gets a refusal rather than a silently different read. The two are deliberately
// separate: correctness lives here, and the error message is a courtesy on top.
func (e *MemQLEngine) stagedScopeFor(ctx context.Context) StagedScope {
	declared := stagedScopeDeclaredOn(ctx)
	if declared.IsEmpty() {
		return StagedScope{}
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return StagedScope{}
	}
	return declared
}

// refuseUnauthorizedStagedScope turns "declared a scope, may not use it" into an
// explicit error. Returns nil when nothing was declared, which is every ordinary
// read, and nil when the declaration was authorized.
//
// Called from executeWith so the refusal happens once, before the parse, rather
// than as a confusingly empty result set. It is NOT what makes the mechanism
// safe -- stagedScopeFor is, by resolving to an empty scope regardless -- so a
// future entry point that forgets this call is wrong about its error message and
// right about its rows.
func (e *MemQLEngine) refuseUnauthorizedStagedScope(ctx context.Context) error {
	declared := stagedScopeDeclaredOn(ctx)
	if declared.IsEmpty() || rowAuthzIsClusterOwner(ctx) {
		return nil
	}
	return &StagedScopeDeniedError{ConceptIds: declared.signature()}
}

// StagedScopeDeniedError is returned when a caller declares a staged-scoped read
// they are not authorized to make. Exported so a transport layer can map it to
// its own permission status rather than matching on a string.
type StagedScopeDeniedError struct {
	// ConceptIds is the sorted, comma-joined declaration that was refused. It
	// names concepts the caller already named, so echoing it discloses nothing
	// they did not supply.
	ConceptIds string
}

func (e *StagedScopeDeniedError) Error() string {
	return "staged-data: an explicitly staged-scoped read of [" + e.ConceptIds +
		"] is a cluster-owner capability, and this caller is not the cluster owner; " +
		"refusing rather than returning an ordinary read, which would be " +
		"indistinguishable from nothing being staged"
}

// stagedConceptWithheld reports whether rows of conceptId must be withheld from
// a read that resolved `scope`.
//
// THE SINGLE DECISION. Both enforcement seams reach it: the plan-time conjunct
// through stagedConceptIds, and the per-row gate through admitStagedRow. Anything
// that computed this another way would disagree with one of the two, and
// memql#4040 names that outcome -- a read returning some staged rows and not
// others -- as worse than either answer on its own.
//
// Order of the two terms matters for cost, not for meaning: conceptDataIsStaged
// is one sync.Map load and is false for every concept on essentially every
// installation, so the scope lookup is never reached in the common case.
func (e *MemQLEngine) stagedConceptWithheld(scope StagedScope, conceptId string) bool {
	conceptId = strings.TrimSpace(conceptId)
	return e.conceptDataIsStaged(conceptId) && !scope.includes(conceptId)
}
