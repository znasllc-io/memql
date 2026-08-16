// Package authoring is the Go SDK surface for memQL's authoring operations --
// the cockpit-facing way to VALIDATE and SESSION-DEFINE a user "bundle" (a set
// of .memql sources) over the engine's gRPC stream (issue memql#2128 / C1).
//
// Two operations, both reusing the engine's existing authoring machinery
// (component/memql/authoring_sandbox.go + authoring_session.go):
//
//   - ValidateBundle runs the Gate-1 isolated compile-and-bind sandbox and
//     returns per-construct diagnostics + an overall OK. It NEVER mutates engine
//     state -- safe to call against a running engine.
//   - SessionDefineBundle validates, then registers the bundle into the caller's
//     owner-scoped, STREAM-scoped authored registry. Defined function-family
//     constructs become callable BY NAME for the lifetime of the stream, never
//     shadowing core, and are dropped when the stream (session) ends.
//
// A third operation adds the owner-gated DURABLE promotion (issue
// znasllc-io/memql-cockpit#232):
//
//   - DurablePromoteBundle validates the bundle, then durably promotes every
//     plain construct (query / mutation / logic / spec / trait) into the SHARED
//     engine registry: persisted (reviewable + restart-durable), callable by
//     every session, and -- via a live cross-node broadcast -- callable on every
//     node within seconds with no restart. OWNER-only.
//
// The package mirrors sdk/go/sense in shape -- a Client wrapping a
// client.Dispatcher, typed SDK-owned values (no memqlv1.* leaks into the public
// surface), and errors that bubble up. See sdk/go/CLAUDE.md for the broader SDK
// rules (opaque types, named-primitive contract, generated-code-is-read-only).
package authoring

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Client exposes the authoring operations over a Dispatcher. Safe to reuse
// across goroutines once constructed; the underlying Dispatcher is the
// multiplex point.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires an authoring client to the supplied dispatcher. The
// dispatcher must already be Run(); Client.* methods just dispatch SendAndWait
// calls through it.
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// Diagnostic is one entry in a validate / session-define result -- the
// per-construct compile-and-bind outcome. OK is true when the construct
// compiled + bound cleanly. Skipped is true when the sandbox does not yet
// compile the kind (the construct was neither validated nor rejected and does
// NOT fail the bundle). Error carries the compile/bind failure or the skip
// reason.
type Diagnostic struct {
	Name    string
	Kind    string
	OK      bool
	Skipped bool
	Error   string
	// Line / Column / EndLine / EndColumn are the OPTIONAL 1-based authored
	// position of the failing token in bundle-file coordinates (#2375). All
	// zero when no reliable position was computed -- treat 0 as absent, never
	// as line 0. End* are zero when the failure has no end anchor.
	Line      int
	Column    int
	EndLine   int
	EndColumn int
}

// Construct names one construct that a session-define registered (now callable
// by name within the session).
type Construct struct {
	Kind string
	Name string
}

// ValidateResult is the response from ValidateBundle: an overall OK plus the
// per-construct diagnostics. OK is true only when every non-skipped construct
// compiled cleanly. No engine state was mutated.
type ValidateResult struct {
	OK          bool
	Diagnostics []Diagnostic
}

// SessionDefineResult is the response from SessionDefineBundle. On success OK is
// true and Defined lists the constructs that became callable by name for the
// session. On failure OK is false, Error explains the rejection (validation or
// register), and Diagnostics carries the per-construct detail; nothing was
// registered.
type SessionDefineResult struct {
	OK          bool
	Defined     []Construct
	Diagnostics []Diagnostic
	Error       string
}

// ValidateBundle runs the Gate-1 sandbox over the supplied .memql bundle and
// returns the per-construct diagnostics + overall OK. It NEVER mutates engine
// state. The Go error return is reserved for wire-level failures (dispatcher
// closed, context cancelled, permission denied) -- a bundle that fails to
// compile comes back as OK=false with diagnostics, not as an error.
func (c *Client) ValidateBundle(ctx context.Context, sources string) (*ValidateResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AuthoringValidateBundle{
			AuthoringValidateBundle: &memqlv1.AuthoringValidateBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.validateBundle: %w", err)
	}
	result := resp.GetAuthoringValidateBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.validateBundle: empty response")
	}
	return &ValidateResult{
		OK:          result.GetOk(),
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
	}, nil
}

// SessionDefineBundle validates the bundle, then session-defines it into the
// caller's owner-scoped, stream-scoped authored registry. Defined
// function-family constructs become callable by name within the session, never
// shadowing core, dropped when the stream ends. The Go error return is reserved
// for wire-level failures; a bundle the engine rejected comes back as OK=false
// with a populated Error + diagnostics.
func (c *Client) SessionDefineBundle(ctx context.Context, sources string) (*SessionDefineResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AuthoringSessionDefineBundle{
			AuthoringSessionDefineBundle: &memqlv1.AuthoringSessionDefineBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.sessionDefineBundle: %w", err)
	}
	result := resp.GetAuthoringSessionDefineBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.sessionDefineBundle: empty response")
	}
	defined := make([]Construct, 0, len(result.GetDefined()))
	for _, d := range result.GetDefined() {
		defined = append(defined, Construct{Kind: d.GetKind(), Name: d.GetName()})
	}
	return &SessionDefineResult{
		OK:          result.GetOk(),
		Defined:     defined,
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
		Error:       result.GetError(),
	}, nil
}

// ConceptSchemaChange is ONE classified difference between the version of a
// concept the cluster is running and the version a promote is putting over it
// (memql#3757).
//
// Breaking is the classification the engine made, not one to re-derive here: a
// removed field, a changed field type, a new required field and a narrowed enum
// are breaking; everything else lands. RowsAffected is a REAL count taken
// against the live table, and is only meaningful when RowCountKnown is true --
// zero from a node that could not count would be a claim it is not entitled to
// make, so the two are separate fields rather than a sentinel.
type ConceptSchemaChange struct {
	Concept string
	// Field is the payload field, dotted for one inside a nested block
	// ("preferences.theme"). Empty for a concept-level change.
	Field string
	// Kind is a closed set: field_added, field_removed, field_type_changed,
	// field_required_added, field_required_dropped, enum_widened, enum_narrowed,
	// description_changed, relationship_added, relationship_removed,
	// node_type_changed.
	Kind          string
	Breaking      bool
	Was           string
	Now           string
	RowsAffected  int64
	RowCountKnown bool
	// ReferencedBy names the loaded constructs the change reaches, as
	// "kind:name" ("query:spaceParticipants").
	ReferencedBy []string
	// Detail is the engine's one-line reason, already carrying the counts.
	Detail string
}

// ConceptSchemaDiff is the whole classification of one concept re-promote.
// Overridden records that a breaking change landed because AllowBreaking was
// passed; Summary is the engine's own rendered block, provided so a caller can
// show the engine's wording rather than rebuild it.
type ConceptSchemaDiff struct {
	Concept    string
	Breaking   bool
	Overridden bool
	Changes    []ConceptSchemaChange
	Summary    string
}

// PromoteResult is the response from DurablePromoteBundle. On success OK is true
// and Promoted lists the constructs durably promoted into the shared registry
// (persisted, restart-durable, and -- via the live broadcast -- callable on every
// node within seconds). On failure OK is false, Error explains the rejection
// (validation or a per-construct promote failure), and Diagnostics carries the
// per-construct compile/bind detail.
type PromoteResult struct {
	OK          bool
	Promoted    []Construct
	Diagnostics []Diagnostic
	Error       string
	// ConceptDiffs carries the memql#3757 classification for every CONCEPT the
	// bundle re-promotes over a version the cluster already runs -- on the
	// refusal as well as on success, because a breaking refusal IS a diff.
	// Empty for a first promote and for an unchanged re-promote.
	//
	// Read this rather than parsing Error: Error is prose for a human, and the
	// field name, the row count and the referencing constructs are all here as
	// values.
	ConceptDiffs []ConceptSchemaDiff
}

// PromoteOption adjusts one DurablePromoteBundle call.
//
// OPAQUE ON PURPOSE (memql#3874). The struct wraps its mutator rather than
// being one, so a consumer can hold a PromoteOption and pass it along but
// cannot build one -- the only way to obtain a non-zero value is a constructor
// in this package. A `func(*memqlv1.DurablePromoteBundleMsg)` underlying type
// would have been an escape hatch straight past the boundary this package
// exists to hold: it lets a caller reach the wire message and set a field the
// SDK has not wrapped, which is exactly the moment the boundary is
// load-bearing. The zero value is inert.
type PromoteOption struct {
	apply func(*memqlv1.DurablePromoteBundleMsg)
}

// AllowBreaking lands a CONCEPT re-promote whose schema change would strand rows
// already written -- a removed field, a changed field type, a new required
// field, a narrowed enum (memql#3757).
//
// Without it such a promote is REFUSED, and the refusal names the field, the
// number of rows it affects and the constructs that reference it. With it the
// change lands and the engine AUDITS the override on v1:identity:auditEvent,
// naming the concept and the fields -- so an override is a recorded decision
// rather than a quieter call.
//
// It moves nothing else. Every other refusal a promote can produce (core-first,
// a failed compile, a broken invariant) is unmoved by it, because none of them
// is a judgement about data.
func AllowBreaking() PromoteOption {
	return PromoteOption{
		apply: func(msg *memqlv1.DurablePromoteBundleMsg) { msg.AllowBreaking = true },
	}
}

// WithPromoteOrigin supplies the bundle's TREE-RELATIVE path, e.g.
// "planner/queries.memql".
//
// It is the AMBIENT DOMAIN the engine resolves an unimported bare concept name
// against, and the domain an unannotated concept declaration assembles its
// canonical id under (memql#3800). Without it the engine runs its no-domain
// degrade, which refuses any construct naming a concept short name more than one
// domain declares -- `plan`, `request`, `call`, `invocation` today.
//
// It exists on BOTH halves of the staged lifecycle deliberately (see
// WithStageOrigin). Offering it on stage alone would have built the exact trap
// the engine-side comment warns about, one level up: a bundle that stages
// cleanly and then refuses to train, on identical source.
func WithPromoteOrigin(path string) PromoteOption {
	return PromoteOption{
		apply: func(msg *memqlv1.DurablePromoteBundleMsg) { msg.Origin = path },
	}
}

// StageResult is the outcome of a durable STAGE (epic memql#3928).
//
// Staged names the constructs now callable BY THEIR AUTHOR -- a different field
// from PromoteResult.Promoted on purpose, so a caller reading a result cannot
// mistake one tier for the other by reaching for the field they expected.
//
// There is no ConceptDiffs, and its absence is structural: a bundle declaring a
// concept is refused outright, so there is never a schema classification here.
type StageResult struct {
	OK          bool
	Staged      []Construct
	Diagnostics []Diagnostic
	Error       string
}

// StageOption adjusts one StageBundle call.
//
// OPAQUE ON PURPOSE, the same shape and for the same reason as PromoteOption
// (memql#3874): the struct WRAPS its mutator rather than being one, so a
// consumer can hold a StageOption and pass it along but cannot build one. A
// `func(*memqlv1.StageBundleMsg)` underlying type would be an escape hatch
// straight past the boundary this package exists to hold. The zero value is
// inert.
type StageOption struct {
	apply func(*memqlv1.StageBundleMsg)
}

// WithStageOrigin supplies the bundle's TREE-RELATIVE path, exactly as
// WithPromoteOrigin does for a promote and for the same reason: a stage runs
// the SAME Gate-1 sandbox a validate does, so omitting it here would refuse
// source that validates.
func WithStageOrigin(path string) StageOption {
	return StageOption{
		apply: func(msg *memqlv1.StageBundleMsg) { msg.Origin = path },
	}
}

// StageBundle validates the bundle, then durably STAGES every stageable
// construct (query / mutation / logic / spec / trait) into the caller's OWN
// tier: persisted, reviewable, replayed at boot -- and resolvable for the
// author and for nobody else (epic memql#3928).
//
// THE MIDDLE TIER. SessionDefineBundle is owner-scoped and dies with the
// stream; DurablePromoteBundle is persisted, shared and broadcast to every
// node. Staging is the second of those with one step omitted -- the cross-node
// broadcast -- and the omission is the tier.
//
// OWNER-OR-DEVELOPER, the same bar ValidateBundle and SessionDefineBundle take
// rather than the durable pair's owner-only. Staging registers nothing shared,
// so its blast radius is a database row rather than a change to what the
// cluster runs.
//
// TO TRAIN A STAGED CONSTRUCT, CALL DurablePromoteBundle WITH THE SAME SOURCE.
// There is no TrainBundle, and its absence is the design: the engine sees the
// construct is staged for this owner and flips the same persisted row rather
// than writing a second one, so a caller never has to know which tier a
// construct is in before it can pick a verb.
//
// A CONCEPT IN THE BUNDLE IS REFUSED, by name, and NOTHING is staged -- not even
// the constructs that would have been fine. A concept registers into the one
// shared concept registry and its merge rebuilds relationship state
// cluster-wide, so there is no owner-scoped form of it to stage.
//
// The Go error return is reserved for wire-level failures; a bundle the engine
// rejected comes back as OK=false with a populated Error + diagnostics.
func (c *Client) StageBundle(ctx context.Context, sources string, opts ...StageOption) (*StageResult, error) {
	body := &memqlv1.StageBundleMsg{Sources: sources}
	for _, opt := range opts {
		if opt.apply != nil {
			opt.apply(body)
		}
	}
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_StageBundle{StageBundle: body},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.stageBundle: %w", err)
	}
	result := resp.GetStageBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.stageBundle: empty response")
	}
	staged := make([]Construct, 0, len(result.GetStaged()))
	for _, s := range result.GetStaged() {
		staged = append(staged, Construct{Kind: s.GetKind(), Name: s.GetName()})
	}
	return &StageResult{
		OK:          result.GetOk(),
		Staged:      staged,
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
		Error:       result.GetError(),
	}, nil
}

// DurablePromoteBundle validates the bundle, then durably promotes every plain
// construct (query / mutation / logic / spec / trait) into the SHARED engine
// registry. The promoted constructs are persisted (reviewable + restart-durable),
// callable by every session, and -- via a live cross-node broadcast -- callable
// on every node within seconds with no restart. OWNER-only: the engine rejects a
// non-owner caller with a permission-denied wire error.
//
// The Go error return is reserved for wire-level failures (dispatcher closed,
// context cancelled, permission denied); a bundle the engine rejected on
// validation comes back as OK=false with a populated Error + diagnostics.
//
// A CONCEPT the bundle re-promotes over a version the cluster already runs is
// classified (memql#3757): additive changes land, a BREAKING one is refused, and
// either way the classification comes back on PromoteResult.ConceptDiffs. Pass
// AllowBreaking() to land a breaking change deliberately.
//
// The options are variadic rather than a second method or a bare bool, for two
// reasons: `DurablePromoteBundle(ctx, src, true)` at a call site says nothing
// about what is being allowed, and a parallel `DurablePromoteBundleAllowing`
// would make the dangerous call the one with the shorter, more obvious name.
func (c *Client) DurablePromoteBundle(ctx context.Context, sources string, opts ...PromoteOption) (*PromoteResult, error) {
	body := &memqlv1.DurablePromoteBundleMsg{Sources: sources}
	for _, opt := range opts {
		if opt.apply != nil {
			opt.apply(body)
		}
	}
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_DurablePromoteBundle{
			DurablePromoteBundle: body,
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.durablePromoteBundle: %w", err)
	}
	result := resp.GetDurablePromoteBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.durablePromoteBundle: empty response")
	}
	promoted := make([]Construct, 0, len(result.GetPromoted()))
	for _, p := range result.GetPromoted() {
		promoted = append(promoted, Construct{Kind: p.GetKind(), Name: p.GetName()})
	}
	return &PromoteResult{
		OK:           result.GetOk(),
		Promoted:     promoted,
		Diagnostics:  protoDiagnostics(result.GetDiagnostics()),
		Error:        result.GetError(),
		ConceptDiffs: protoConceptDiffs(result.GetConceptDiffs()),
	}, nil
}

// DemoteOutcomeRetired / DemoteOutcomeRemoved are the two values
// DemoteOutcome.Outcome takes. Compare against these rather than the string
// literals: they are the SDK's own copy of the engine vocabulary, which is what
// keeps a consumer off the wire types.
const (
	// DemoteOutcomeRetired: the construct is still REGISTERED and its name is
	// still claimed. Only a concept with rows under it reaches this -- its rows
	// stay readable and new writes to it are refused until it is re-promoted.
	DemoteOutcomeRetired = "retired"
	// DemoteOutcomeRemoved: the construct is gone from the shared registry and
	// its name is claimable again.
	DemoteOutcomeRemoved = "removed"
)

// DemoteOutcome is what actually happened to ONE construct in a demote
// (memql#3756).
//
// A concept is why this is not implied by success. Rows written under a concept
// outlive its definition, so demoting one with rows under it RETIRES it --
// registered, readable, closed to new writes -- and only demoting one with zero
// rows REMOVES it and frees the name. Both are OK=true, so the outcome is
// reported rather than inferred: whether the name is claimable again is the next
// thing the caller needs to know, and no reading of "it worked" establishes it.
type DemoteOutcome struct {
	// Kind and Name are the construct as the SOURCE named it (for a concept,
	// Name is the declaration name, not the canonical id).
	Kind string
	Name string
	// ConceptId is the canonical concept id the outcome applies to; empty for
	// every non-concept kind.
	ConceptId string
	// Outcome is DemoteOutcomeRetired or DemoteOutcomeRemoved.
	Outcome string
	// RowCount is the number of DISTINCT rows the engine found under the
	// concept, across every owner -- the evidence that chose the outcome. Always
	// 0 for a non-concept kind, which has no rows of its own.
	RowCount int64
}

// protoConceptDiffs adapts the wire classification into the SDK form. Lives here
// (unexported) so no memqlv1 type leaks across the package boundary, exactly
// like protoDiagnostics -- and it copies field for field, deriving nothing: the
// engine already decided what is breaking and counted the rows, and an SDK that
// recomputed either would be a second authority on the same question.
func protoConceptDiffs(in []*memqlv1.ConceptSchemaDiff) []ConceptSchemaDiff {
	if len(in) == 0 {
		return nil
	}
	out := make([]ConceptSchemaDiff, 0, len(in))
	for _, d := range in {
		changes := make([]ConceptSchemaChange, 0, len(d.GetChanges()))
		for _, c := range d.GetChanges() {
			changes = append(changes, ConceptSchemaChange{
				Concept:       c.GetConcept(),
				Field:         c.GetField(),
				Kind:          c.GetKind(),
				Breaking:      c.GetBreaking(),
				Was:           c.GetWas(),
				Now:           c.GetNow(),
				RowsAffected:  c.GetRowsAffected(),
				RowCountKnown: c.GetRowCountKnown(),
				ReferencedBy:  c.GetReferencedBy(),
				Detail:        c.GetDetail(),
			})
		}
		out = append(out, ConceptSchemaDiff{
			Concept:    d.GetConcept(),
			Breaking:   d.GetBreaking(),
			Overridden: d.GetOverridden(),
			Changes:    changes,
			Summary:    d.GetSummary(),
		})
	}
	return out
}

// DemoteResult is the response from DurableDemoteBundle, the inverse of
// PromoteResult. On success OK is true, Demoted lists the constructs durably
// demoted out of the shared registry, and Outcomes says what happened to each of
// them. The withdrawal is persisted (so a restart replays it) and broadcast (so
// every node applies it within seconds). On failure OK is false and Error
// explains the rejection (no demotable constructs in the source, or a
// per-construct demote failure).
type DemoteResult struct {
	OK      bool
	Demoted []Construct
	// Outcomes carries the same constructs as Demoted, in the same order, each
	// with the outcome that applied to it. Read this -- not the presence of a
	// name in Demoted -- to learn whether a demoted concept's name is free again.
	Outcomes    []DemoteOutcome
	Diagnostics []Diagnostic
	Error       string
}

// DurableDemoteBundle durably demotes every construct (concept / query /
// mutation / logic / spec / trait) named in the bundle source OUT of the SHARED
// engine registry -- the inverse of DurablePromoteBundle (memql#2163). The
// withdrawal is persisted (so a restart replays it), applied to the shared
// registry on this node (author-promoted-only safety gate -- a core construct can
// never be removed), and -- via a live cross-node broadcast -- applied on every
// node within seconds with no restart. OWNER-only: the engine rejects a non-owner
// caller with a permission-denied wire error.
//
// CHECK Outcomes, not just OK (memql#3756). A CONCEPT with rows under it is
// RETIRED rather than removed: it stays registered so those rows stay readable,
// new writes to it are refused, and its name stays claimed until it is
// re-promoted. Only a concept with zero rows is removed and its name freed. Both
// come back OK=true.
//
// A demote needs only the names + kinds the source declares; it never compiles
// the construct, so a construct whose source no longer compiles can still be
// demoted. The Go error return is reserved for wire-level failures (dispatcher
// closed, context cancelled, permission denied); a demote the engine rejected
// comes back as OK=false with a populated Error.
func (c *Client) DurableDemoteBundle(ctx context.Context, sources string) (*DemoteResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_DurableDemoteBundle{
			DurableDemoteBundle: &memqlv1.DurableDemoteBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.durableDemoteBundle: %w", err)
	}
	result := resp.GetDurableDemoteBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.durableDemoteBundle: empty response")
	}
	demoted := make([]Construct, 0, len(result.GetDemoted()))
	for _, d := range result.GetDemoted() {
		demoted = append(demoted, Construct{Kind: d.GetKind(), Name: d.GetName()})
	}
	outcomes := make([]DemoteOutcome, 0, len(result.GetOutcomes()))
	for _, o := range result.GetOutcomes() {
		outcomes = append(outcomes, DemoteOutcome{
			Kind:      o.GetKind(),
			Name:      o.GetName(),
			ConceptId: o.GetConceptId(),
			Outcome:   o.GetOutcome(),
			RowCount:  o.GetRowCount(),
		})
	}
	return &DemoteResult{
		OK:          result.GetOk(),
		Demoted:     demoted,
		Outcomes:    outcomes,
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
		Error:       result.GetError(),
	}, nil
}

// protoDiagnostics adapts the wire diagnostics into the SDK form. Lives here
// (unexported) so no memqlv1 type leaks across the package boundary.
func protoDiagnostics(in []*memqlv1.AuthoringDiagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		out = append(out, Diagnostic{
			Name:      d.GetName(),
			Kind:      d.GetKind(),
			OK:        d.GetOk(),
			Skipped:   d.GetSkipped(),
			Error:     d.GetError(),
			Line:      int(d.GetLine()),
			Column:    int(d.GetColumn()),
			EndLine:   int(d.GetEndLine()),
			EndColumn: int(d.GetEndColumn()),
		})
	}
	return out
}
