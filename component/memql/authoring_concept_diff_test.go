package memql

// authoring_concept_diff_test.go -- coverage for classifying a concept SCHEMA
// CHANGE on re-promote (memql#3757).
//
// The tests are in three layers, and the split is deliberate.
//
//  1. The RULES, against the pure classifier. No engine, no registry, no
//     database -- two compiled concepts in, a classification out. Each of the
//     eight cases the design fixed gets its own assertion, so a rule that
//     regresses names itself instead of failing a composite.
//  2. The GATE, through the real promote path on a real registry. That a change
//     is classified correctly says nothing about whether the promote path
//     consults the classification, refuses on it, leaves the registry untouched
//     when it does, and skips it entirely on a first promote and on the replay
//     paths. Those are properties of the WIRING, and the layer-1 tests would
//     pass against an implementation that computed a beautiful diff and ignored
//     it.
//  3. The NUMBERS, against a live Postgres. The acceptance criterion is that the
//     refusal reports a REAL row count and REAL referencing constructs -- and a
//     count is exactly the kind of claim that passes every test written without
//     the thing being counted. So the DB layer writes rows, then asserts the
//     refusal names how many there are.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// --- fixtures -------------------------------------------------------------

// diffWidgetV1 is the version a cluster is running in every test below. Every
// v2 differs from it in exactly ONE way, so a classification can only be
// explained by the change it is named for.
const diffWidgetV1 = `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")
}`

const diffWidgetId = "v1:diffns:diffWidget"

// compileConceptForDiff compiles a concept source the way the promote path
// does, so the classifier is fed exactly what it is fed in production -- the
// built *memoryNodes.Concept, not a hand-assembled one that could disagree with
// what the parser actually emits.
func compileConceptForDiff(t *testing.T, source string) *memoryNodes.Concept {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", source, ""); err != nil {
		t.Fatalf("author concept fixture: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "concept", "diffWidget")
	if !ok {
		t.Fatal("concept fixture did not register")
	}
	built, ok := c.Compiled.(*memoryNodes.Concept)
	if !ok || built == nil {
		t.Fatalf("concept fixture compiled to %#v, want a *memoryNodes.Concept", c.Compiled)
	}
	return built
}

// classify runs the pure classifier over two sources.
func classify(t *testing.T, priorSrc, candidateSrc string) ConceptSchemaDiff {
	t.Helper()
	diff, err := classifyConceptSchemaChange(
		compileConceptForDiff(t, priorSrc),
		compileConceptForDiff(t, candidateSrc),
	)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	return diff
}

// changeFor returns the classified change of a given kind, or fails naming what
// was found instead -- so a wrong classification reads as "got enum_widened,
// wanted enum_narrowed" rather than as a nil dereference.
func changeFor(t *testing.T, diff ConceptSchemaDiff, kind string) ConceptSchemaChange {
	t.Helper()
	var kinds []string
	for _, c := range diff.Changes {
		if c.Kind == kind {
			return c
		}
		kinds = append(kinds, c.Kind+"("+c.Field+")")
	}
	t.Fatalf("no %s change in the diff; got %v", kind, kinds)
	return ConceptSchemaChange{}
}

// --- layer 1: the rules ---------------------------------------------------

// TestClassifyConceptSchemaChange_AdditiveLands walks every change the design
// calls additive. Each must land, and each must still be REPORTED -- a promote
// that succeeds still owes the author an account of what it changed.
func TestClassifyConceptSchemaChange_AdditiveLands(t *testing.T) {
	cases := []struct {
		name      string
		candidate string
		wantKind  string
	}{
		{
			name: "a new optional field",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")
  region       string
}`,
			wantKind: ConceptChangeFieldAdded,
		},
		{
			name: "a new @relationship",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")

  @relationship(type="parent", field="ownerUserId", target="v1:identity:user", direction="outgoing")
}`,
			wantKind: ConceptChangeRelationshipAdded,
		},
		{
			name: "an edited @description",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget with a better sentence about it")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")
}`,
			wantKind: ConceptChangeDescriptionChanged,
		},
		{
			name: "a widened @enum",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived", "pending")
}`,
			wantKind: ConceptChangeEnumWidened,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := classify(t, diffWidgetV1, tc.candidate)
			if diff.Breaking {
				t.Fatalf("%s classified BREAKING; the design calls it additive.\n%s", tc.name, renderConceptSchemaDiff(diff))
			}
			change := changeFor(t, diff, tc.wantKind)
			if change.Breaking {
				t.Errorf("%s change is marked breaking", tc.wantKind)
			}
		})
	}
}

// TestClassifyConceptSchemaChange_BreakingIsRefused walks every change the
// design calls breaking. Each must be classified breaking AND must name the
// field, because "this promote is breaking" without a field is a refusal an
// operator cannot act on.
func TestClassifyConceptSchemaChange_BreakingIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		candidate string
		wantKind  string
		wantField string
	}{
		{
			name: "a removed field",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  price        string
  status       enum("draft", "active", "archived")
}`,
			wantKind:  ConceptChangeFieldRemoved,
			wantField: "sku",
		},
		{
			name: "a changed field type",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        float
  status       enum("draft", "active", "archived")
}`,
			wantKind:  ConceptChangeFieldTypeChanged,
			wantField: "price",
		},
		{
			name: "a new required field",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")
  region       string  @required
}`,
			wantKind:  ConceptChangeFieldRequiredAdded,
			wantField: "region",
		},
		{
			name: "an existing optional field made required",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string  @required
  price        string
  status       enum("draft", "active", "archived")
}`,
			wantKind:  ConceptChangeFieldRequiredAdded,
			wantField: "sku",
		},
		{
			name: "a narrowed @enum",
			candidate: `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active")
}`,
			wantKind:  ConceptChangeEnumNarrowed,
			wantField: "status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := classify(t, diffWidgetV1, tc.candidate)
			if !diff.Breaking {
				t.Fatalf("%s classified as additive; the design refuses it.\n%s", tc.name, renderConceptSchemaDiff(diff))
			}
			change := changeFor(t, diff, tc.wantKind)
			if !change.Breaking {
				t.Errorf("%s change is not marked breaking", tc.wantKind)
			}
			if change.Field != tc.wantField {
				t.Errorf("change names field %q, want %q -- a refusal that does not name the field is not actionable", change.Field, tc.wantField)
			}
		})
	}
}

// TestClassifyConceptSchemaChange_IdenticalSchemaIsNoChange: re-promoting the
// same source reports nothing. It matters because the diff appears on every
// promote reply, and a diff that said something about an unchanged concept would
// read to a client as "we looked and could not tell".
func TestClassifyConceptSchemaChange_IdenticalSchemaIsNoChange(t *testing.T) {
	diff := classify(t, diffWidgetV1, diffWidgetV1)
	if len(diff.Changes) != 0 || diff.Breaking {
		t.Fatalf("re-promoting an identical schema reported %d changes (breaking=%v):\n%s",
			len(diff.Changes), diff.Breaking, renderConceptSchemaDiff(diff))
	}
}

// TestClassifyConceptSchemaChange_OptionalDatetimeMadeRequiredIsOneChange pins
// the trap in reading the emitted JSON Schema instead of the authored type.
//
// An author's `datetime` emits two structurally different schemas depending on
// whether it is required: `type: string, format: date-time` when it is, and a
// three-member oneOf sentinel when it is not (memql#1629). A classifier reading
// those naively reports a TYPE change on top of the required change -- and the
// type half is a lie, because the author changed no type. One change, not two.
func TestClassifyConceptSchemaChange_OptionalDatetimeMadeRequiredIsOneChange(t *testing.T) {
	prior := `@version("1.0.0")
@namespace("diffns")
concept diffWidget {
  ownerUserId  string  @required
  expiresAt    datetime
}`
	candidate := `@version("1.0.0")
@namespace("diffns")
concept diffWidget {
  ownerUserId  string  @required
  expiresAt    datetime  @required
}`
	diff := classify(t, prior, candidate)
	for _, c := range diff.Changes {
		if c.Kind == ConceptChangeFieldTypeChanged {
			t.Fatalf("flipping an optional datetime to required reported a TYPE change (%s -> %s); "+
				"the emitted schema differs but the author's type did not", c.Was, c.Now)
		}
	}
	changeFor(t, diff, ConceptChangeFieldRequiredAdded)
}

// TestClassifyConceptSchemaChange_NestedFieldRemovalIsBreaking: the same four
// rules one level down. A nested block's `!` is enforced at insert (memql#3623)
// and its keys are closed (memql#3641), so a nested field is as real as a
// top-level one and removing it is as breaking.
func TestClassifyConceptSchemaChange_NestedFieldRemovalIsBreaking(t *testing.T) {
	prior := `@version("1.0.0")
@namespace("diffns")
concept diffWidget {
  ownerUserId  string  @required
  preferences {
    theme   string
    locale  string
  }
}`
	candidate := `@version("1.0.0")
@namespace("diffns")
concept diffWidget {
  ownerUserId  string  @required
  preferences {
    theme   string
  }
}`
	diff := classify(t, prior, candidate)
	if !diff.Breaking {
		t.Fatalf("removing a nested field classified as additive:\n%s", renderConceptSchemaDiff(diff))
	}
	change := changeFor(t, diff, ConceptChangeFieldRemoved)
	if change.Field != "preferences.locale" {
		t.Errorf("nested removal names field %q, want the dotted path %q", change.Field, "preferences.locale")
	}
}

// TestRenderConceptSchemaDiff_ShowsTheShapeTheDesignSpecified: the refusal is
// read by a person deciding whether to override it, so its shape is part of the
// contract, not decoration. Markers, the field, both sides of a type change, and
// the counts.
func TestRenderConceptSchemaDiff_ShowsTheShapeTheDesignSpecified(t *testing.T) {
	diff := ConceptSchemaDiff{
		Concept:  diffWidgetId,
		Breaking: true,
		Changes: []ConceptSchemaChange{
			{
				Concept: diffWidgetId, Field: "sku", Kind: ConceptChangeFieldRemoved, Breaking: true,
				Was: "string", RowsAffected: 1284, RowCountKnown: true,
				ReferencedBy: []string{"query:a", "query:b", "query:c"},
			},
			{
				Concept: diffWidgetId, Field: "price", Kind: ConceptChangeFieldTypeChanged, Breaking: true,
				Was: "string", Now: "float", RowsAffected: 1284, RowCountKnown: true,
			},
			{
				Concept: diffWidgetId, Field: "region", Kind: ConceptChangeFieldRequiredAdded, Breaking: true,
				Was: "absent", Now: "string!", RowsAffected: 1284, RowCountKnown: true,
				ReferencedBy: []string{"mutation:createWidget", "mutation:updateWidget"},
			},
		},
	}
	for i := range diff.Changes {
		diff.Changes[i].Detail = describeConceptSchemaChange(diff.Changes[i])
	}
	got := renderConceptSchemaDiff(diff)

	for _, want := range []string{
		"BREAKING - refused",
		"- sku",
		"removed; 1,284 rows carry it, 3 queries reference it",
		"~ price string -> float",
		"+ region string!",
		"required; 1,284 rows lack it, 2 mutations do not supply it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal block is missing %q:\n%s", want, got)
		}
	}
}

// TestRowClause_NeverInventsAZeroItDidNotMeasure: an unknown count must read as
// unknown. Rendering "0 rows carry it" from a node that could not look is the
// exact shape of a checker making a claim about the code when it is really
// making one about itself.
func TestRowClause_NeverInventsAZeroItDidNotMeasure(t *testing.T) {
	c := ConceptSchemaChange{Field: "sku", Kind: ConceptChangeFieldRemoved, Breaking: true, RowCountKnown: false}
	got := describeConceptSchemaChange(c)
	if strings.Contains(got, "0 rows") {
		t.Fatalf("an unmeasured count rendered as a zero: %q", got)
	}
	if !strings.Contains(got, "row count unavailable") {
		t.Fatalf("an unmeasured count did not say so: %q", got)
	}
}

// --- layer 2: the gate ----------------------------------------------------

// promoteDiffConcept promotes a concept source through the durable path with an
// explicit gate, returning the store so a test can assert on what was (or was
// not) persisted.
func promoteDiffConcept(t *testing.T, e *MemQLEngine, source string, gate *conceptPromoteGate) (*fakePromoteStore, error) {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", source, ""); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "concept", "diffWidget")
	if !ok {
		t.Fatal("session define did not register the concept")
	}
	store := &fakePromoteStore{}
	return store, e.promoteConstructDurableWithStore(context.Background(), store, gate, "owner-1", c)
}

const diffWidgetV2FieldRemoved = `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  price        string
  status       enum("draft", "active", "archived")
}`

// TestPromoteConcept_FirstPromoteIsNotClassified: the acceptance criterion "a
// first promote (no prior version) is not run through the classifier at all".
//
// It is asserted through the OBSERVABLE consequence -- no diff on the result --
// because that is what a client sees and what the next issue in the epic
// consumes. The structural half is that the hook lives inside the branch where
// the registry already holds the id, so there is no first-promote path through
// the classifier to take.
func TestPromoteConcept_FirstPromoteIsNotClassified(t *testing.T) {
	e := promoteConceptEngine(t)
	gate := &conceptPromoteGate{}
	if _, err := promoteDiffConcept(t, e, diffWidgetV1, gate); err != nil {
		t.Fatalf("first promote of a concept: %v", err)
	}
	if diffs := gate.collected(); len(diffs) != 0 {
		t.Fatalf("a FIRST promote produced %d schema diffs (%+v); there was no prior version to diff against", len(diffs), diffs)
	}
}

// TestPromoteConcept_BreakingChangeIsRefusedAndChangesNothing: the whole point
// of the gate.
//
// The two assertions after the refusal are the ones that would still pass
// against a broken implementation if they were omitted: the LIVE REGISTRY must
// still hold the old version (a refusal that merged first and complained after
// would have already changed the cluster), and NOTHING must have been persisted
// (a reviewable row for a promote that did not happen is a lie in the audit
// trail, and its bundle id would be broadcast to every peer).
func TestPromoteConcept_BreakingChangeIsRefusedAndChangesNothing(t *testing.T) {
	e := promoteConceptEngine(t)
	if _, err := promoteDiffConcept(t, e, diffWidgetV1, &conceptPromoteGate{}); err != nil {
		t.Fatalf("first promote: %v", err)
	}

	store, err := promoteDiffConcept(t, e, diffWidgetV2FieldRemoved, &conceptPromoteGate{})
	if err == nil {
		t.Fatal("re-promoting a concept with a REMOVED field was allowed; it must be refused")
	}
	if !strings.Contains(err.Error(), "sku") {
		t.Errorf("refusal does not name the removed field: %v", err)
	}

	// The refusal must survive the promote path's error wrapping as a TYPED
	// error, because that is how the diff reaches the wire structurally. A
	// refusal that only rendered into a string would leave every client
	// regexing a field name out of prose.
	var breaking *ConceptSchemaBreakingError
	if !errors.As(err, &breaking) {
		t.Fatalf("refusal is not a *ConceptSchemaBreakingError, so the gRPC layer has nothing structural to put on the reply: %T", err)
	}
	if !breaking.Diff.Breaking || breaking.Diff.Concept != diffWidgetId {
		t.Errorf("carried diff = %+v, want breaking=true concept=%q", breaking.Diff, diffWidgetId)
	}

	live, gerr := e.concepts.Get(diffWidgetId)
	if gerr != nil {
		t.Fatalf("the refused promote removed the concept from the registry: %v", gerr)
	}
	fields, ferr := flattenConceptFields(live)
	if ferr != nil {
		t.Fatalf("read the live concept's fields: %v", ferr)
	}
	if _, stillThere := fields["sku"]; !stillThere {
		t.Error("the REFUSED promote was merged anyway -- the live concept lost `sku`")
	}
	if len(store.bundles) != 0 || len(store.constructs) != 0 {
		t.Errorf("a refused promote persisted %d bundle rows + %d construct rows; the classification must run before anything is written or broadcast",
			len(store.bundles), len(store.constructs))
	}
}

// TestPromoteConcept_AdditiveChangeLandsAndIsReported: the other half of the
// rule. An added optional field goes in, the registry carries it afterwards, and
// the diff still reports it.
func TestPromoteConcept_AdditiveChangeLandsAndIsReported(t *testing.T) {
	e := promoteConceptEngine(t)
	if _, err := promoteDiffConcept(t, e, diffWidgetV1, &conceptPromoteGate{}); err != nil {
		t.Fatalf("first promote: %v", err)
	}

	widened := `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")
  region       string
}`
	gate := &conceptPromoteGate{}
	if _, err := promoteDiffConcept(t, e, widened, gate); err != nil {
		t.Fatalf("re-promoting with a new OPTIONAL field must land: %v", err)
	}
	live, err := e.concepts.Get(diffWidgetId)
	if err != nil {
		t.Fatalf("concept missing after an additive re-promote: %v", err)
	}
	fields, ferr := flattenConceptFields(live)
	if ferr != nil {
		t.Fatalf("read the live concept's fields: %v", ferr)
	}
	if _, ok := fields["region"]; !ok {
		t.Error("the additive re-promote did not take effect -- the live concept has no `region`")
	}
	diffs := gate.collected()
	if len(diffs) != 1 {
		t.Fatalf("additive re-promote recorded %d diffs, want 1 -- a promote that lands still owes an account of what it changed", len(diffs))
	}
	if diffs[0].Breaking {
		t.Error("the additive diff is marked breaking")
	}
}

// recordingAuditSink captures the audit events the engine emits, so the override
// can be checked for the thing that makes it acceptable at all.
type recordingAuditSink struct{ events []AuthoredAuditEvent }

func (s *recordingAuditSink) EmitAuthoredAudit(_ context.Context, ev AuthoredAuditEvent) {
	s.events = append(s.events, ev)
}

// TestPromoteConcept_OverrideLandsAndIsAudited: the escape valve, and the reason
// it is acceptable to have one.
//
// An override is the one act in this feature that leaves no other record of what
// was overridden -- the refusal that would have named the fields never happened,
// and the registry afterwards shows only the new shape. So the audit event is
// not a nicety: if it does not carry the concept and the changes, nothing does.
func TestPromoteConcept_OverrideLandsAndIsAudited(t *testing.T) {
	e := promoteConceptEngine(t)
	audit := &recordingAuditSink{}
	e.SetAuthoredAuditSink(audit)

	if _, err := promoteDiffConcept(t, e, diffWidgetV1, &conceptPromoteGate{}); err != nil {
		t.Fatalf("first promote: %v", err)
	}

	gate := &conceptPromoteGate{allowBreaking: true}
	if _, err := promoteDiffConcept(t, e, diffWidgetV2FieldRemoved, gate); err != nil {
		t.Fatalf("allow_breaking must LAND the change, not soften the message: %v", err)
	}

	live, err := e.concepts.Get(diffWidgetId)
	if err != nil {
		t.Fatalf("concept missing after an overridden re-promote: %v", err)
	}
	fields, ferr := flattenConceptFields(live)
	if ferr != nil {
		t.Fatalf("read the live concept's fields: %v", ferr)
	}
	if _, stillThere := fields["sku"]; stillThere {
		t.Error("the override did not land -- the live concept still declares `sku`")
	}

	diffs := gate.collected()
	if len(diffs) != 1 || !diffs[0].Breaking || !diffs[0].Overridden {
		t.Fatalf("recorded diffs = %+v, want exactly one breaking+overridden diff", diffs)
	}

	if len(audit.events) != 1 {
		t.Fatalf("the override emitted %d audit events, want exactly 1 -- an override with no audit row is an unrecorded privileged act", len(audit.events))
	}
	ev := audit.events[0]
	if ev.Action != AuditActionConceptSchemaBreakingOverride {
		t.Errorf("audit action = %q, want %q", ev.Action, AuditActionConceptSchemaBreakingOverride)
	}
	if ev.OwnerUserId != "owner-1" {
		t.Errorf("audit actor = %q, want the promoting owner %q", ev.OwnerUserId, "owner-1")
	}
	if got := fmt.Sprint(ev.Detail["concept"]); got != diffWidgetId {
		t.Errorf("audit detail names concept %q, want %q", got, diffWidgetId)
	}
	changes, ok := ev.Detail["changes"].([]ConceptSchemaChange)
	if !ok || len(changes) == 0 {
		t.Fatalf("audit detail carries no classified changes (%T); the audit row is the only record of WHAT was overridden", ev.Detail["changes"])
	}
	var named bool
	for _, c := range changes {
		if c.Field == "sku" && c.Breaking {
			named = true
		}
	}
	if !named {
		t.Error("the audit row does not name the breaking field that was overridden")
	}
}

// TestPromoteConcept_AdditiveChangeIsNotAudited: the audit channel is for the
// override specifically. A promote that the rules allowed is not a privileged
// act, and auditing every re-promote would bury the one row that matters.
func TestPromoteConcept_AdditiveChangeIsNotAudited(t *testing.T) {
	e := promoteConceptEngine(t)
	audit := &recordingAuditSink{}
	e.SetAuthoredAuditSink(audit)

	if _, err := promoteDiffConcept(t, e, diffWidgetV1, &conceptPromoteGate{}); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	additive := `@version("1.0.0")
@namespace("diffns")
@description("A widget whose schema is about to change")
concept diffWidget {
  ownerUserId  string  @required
  sku          string
  price        string
  status       enum("draft", "active", "archived")
  region       string
}`
	if _, err := promoteDiffConcept(t, e, additive, &conceptPromoteGate{allowBreaking: true}); err != nil {
		t.Fatalf("additive re-promote: %v", err)
	}
	if len(audit.events) != 0 {
		t.Fatalf("an additive re-promote emitted %d audit events; the override audit must not fire for a change nothing objected to", len(audit.events))
	}
}

// TestPromoteConcept_ReplayDoesNotReclassify: boot re-hydration and cross-node
// propagation both funnel through recompileAndPromoteRow, and neither may
// re-decide a decision already taken.
//
// The scenario is the one that breaks a fleet: an operator overrode a breaking
// change, so the durable store holds v1 AND v2 as two bundles. The boot walk
// replays them in order. If the walk classified, v2 would be refused against v1
// and the node would silently come back running the OLDER concept -- diverging
// from every peer that had not restarted.
func TestPromoteConcept_ReplayDoesNotReclassify(t *testing.T) {
	e := promoteConceptEngine(t)
	if _, err := promoteDiffConcept(t, e, diffWidgetV1, &conceptPromoteGate{}); err != nil {
		t.Fatalf("seed the prior version: %v", err)
	}

	row := AuthoringConstructRow{
		Id: "c1", BundleId: "b1", OwnerUserId: "owner-1",
		Kind: "concept", Name: "diffWidget", Source: diffWidgetV2FieldRemoved, Status: "active",
	}
	if err := e.recompileAndPromoteRow(context.Background(), row); err != nil {
		t.Fatalf("a REPLAY of a breaking change was refused: %v\n\n"+
			"Re-classifying on replay means a node that restarts silently reverts to the older "+
			"concept while its peers keep the newer one.", err)
	}
	live, gerr := e.concepts.Get(diffWidgetId)
	if gerr != nil {
		t.Fatalf("concept missing after replay: %v", gerr)
	}
	fields, ferr := flattenConceptFields(live)
	if ferr != nil {
		t.Fatalf("read the live concept's fields: %v", ferr)
	}
	if _, stillThere := fields["sku"]; stillThere {
		t.Error("the replayed version did not take effect")
	}
}

// --- layer 3: the numbers (live Postgres) ---------------------------------

// conceptDiffDBEngine boots a REAL engine against a REAL Postgres, the same
// New + Init path app.Run runs.
//
// It binds to the PACKAGE-LEVEL default concept registry and restores it
// afterwards, for the reason promoteConceptEngineOnTheDefaultRegistry records:
// the Gate-1 sandbox is engine-free and clones the package default, so only an
// engine bound to that registry reproduces the production binding a mutation
// needs to resolve a freshly-promoted concept.
func conceptDiffDBEngine(t *testing.T) (*MemQLEngine, context.Context) {
	t.Helper()
	dsn := dbtest.DSN()
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "concept schema-change row count", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := memoryNodes.All()
	t.Cleanup(func() { memoryNodes.ReplaceAll(before) })

	eng, err := New(db)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	ctx := auth.ContextWithUserActor(context.Background(), "owner-1")
	return eng, ctx
}

// promoteOrFail promotes a bundle and fails with the per-construct diagnostics
// folded in. A bare "N of M constructs did not compile" says only THAT a fixture
// was rejected, never why, which is the difference between a five-second fix and
// a bisect.
func promoteOrFail(t *testing.T, eng *MemQLEngine, ctx context.Context, source string) {
	t.Helper()
	res, err := eng.PromoteBundleDurable(ctx, "owner-1", source, "", false)
	if err == nil {
		return
	}
	var detail []string
	for _, d := range res.Diagnostics {
		if !d.OK && strings.TrimSpace(d.Error) != "" {
			detail = append(detail, d.Kind+" "+d.Name+": "+d.Error)
		}
	}
	t.Fatalf("promote bundle: %v\n%s", err, strings.Join(detail, "\n"))
}

// TestPromoteConcept_RefusalReportsRealRowsAndRealConstructs is the acceptance
// criterion that cannot be met without a database: the refusal reports a REAL
// row count and REAL referencing constructs, not a placeholder.
//
// It is written so the instrument is demonstrably able to move. The count is
// asserted against a number the test CHOSE by writing that many rows -- not
// against "> 0", which a hardcoded 1 would satisfy -- and the referencing set is
// asserted to contain a construct the test itself promoted, so an implementation
// that returned an empty list would fail rather than pass quietly.
//
// Every id and namespace is unique per run: this database is shared with other
// sessions, and the test writes rows and never truncates anything.
func TestPromoteConcept_RefusalReportsRealRowsAndRealConstructs(t *testing.T) {
	eng, ctx := conceptDiffDBEngine(t)

	// A namespace nothing else can collide with, so the row count is a count of
	// THIS test's rows and of nothing else in a shared database.
	ns := "difftest" + strings.ToLower(strings.ReplaceAll(id.NewShortId(), "-", ""))[:12]
	conceptId := "v1:" + ns + ":order"

	v1 := fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("An order")
concept order {
  ownerUserId  string  @required
  sku          string
  status       enum("draft", "placed", "shipped")
}`, ns)

	querySrc := fmt.Sprintf(`use %s.concepts.{ order }

@actor
@description("Orders by sku")
query order ordersBySku%s {
  args {
    sku  string  @required
  }
  filter  sku==args.sku && ownerUserId==actor.userId
}`, ns, ns)

	mutationSrc := fmt.Sprintf(`use %s.concepts.{ order }

@actor
@description("Create an order")
mutate order createOrder%s {
  args {
    orderId  string  @required
    sku      string  @required
  }
  insert {
    id:          args.orderId
    ownerUserId: actor.userId
    sku:         args.sku
    status:      "draft"
  }
}`, ns, ns)

	promoteOrFail(t, eng, ctx, v1)
	promoteOrFail(t, eng, ctx, querySrc+"\n\n"+mutationSrc)

	// Write a number the test chose, so "real count" means this number.
	const rows = 7
	for i := 0; i < rows; i++ {
		call := fmt.Sprintf(`mutation createOrder%s(orderId: %q, sku: %q)`, ns, ns+"-order-"+id.NewShortId(), "sku-"+ns)
		if _, err := eng.Execute(ctx, call); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// Confirm the instrument can move BEFORE relying on it: a field nothing
	// carries must count zero, so a later count of 7 is a measurement rather
	// than a constant.
	if got, known := eng.countConceptRowsWithField(ctx, conceptId, "sku", true); !known || got != rows {
		t.Fatalf("row count for a field every row carries = (%d, known=%v), want (%d, true)", got, known, rows)
	}
	if got, known := eng.countConceptRowsWithField(ctx, conceptId, "sku", false); !known || got != 0 {
		t.Fatalf("row count for rows LACKING a field every row carries = (%d, known=%v), want (0, true)", got, known)
	}

	v2 := fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("An order")
concept order {
  ownerUserId  string  @required
  status       enum("draft", "placed", "shipped")
}`, ns)

	res, err := eng.PromoteBundleDurable(ctx, "owner-1", v2, "", false)
	if err == nil {
		t.Fatal("re-promoting with `sku` removed was allowed against a table that holds rows carrying it")
	}
	if len(res.ConceptDiffs) != 1 {
		t.Fatalf("the refusal reply carries %d concept diffs, want 1 -- the classification must reach the client structurally, not only as prose", len(res.ConceptDiffs))
	}
	diff := res.ConceptDiffs[0]
	change := changeFor(t, diff, ConceptChangeFieldRemoved)

	if !change.RowCountKnown {
		t.Fatal("the refusal reports the row count as unknown against a live database")
	}
	if change.RowsAffected != rows {
		t.Errorf("refusal reports %d rows carrying `sku`, want the %d this test wrote", change.RowsAffected, rows)
	}
	wantRef := "query:ordersBySku" + ns
	if !conceptDiffListHas(change.ReferencedBy, wantRef) {
		t.Errorf("refusal's referencing constructs = %v, want it to include %q -- a real reference read off the live registry", change.ReferencedBy, wantRef)
	}
	t.Logf("the refusal an operator sees:\n%s", diff.Summary)
	if !strings.Contains(diff.Summary, "7 rows carry it") {
		t.Errorf("rendered refusal does not carry the real count:\n%s", diff.Summary)
	}

	// The override is the same call with the flag, and it must actually land.
	overridden, err := eng.PromoteBundleDurable(ctx, "owner-1", v2, "", true)
	if err != nil {
		t.Fatalf("allow_breaking must land the change: %v", err)
	}
	if len(overridden.ConceptDiffs) != 1 || !overridden.ConceptDiffs[0].Overridden {
		t.Fatalf("overridden reply = %+v, want one diff marked overridden", overridden.ConceptDiffs)
	}
	live, gerr := eng.concepts.Get(conceptId)
	if gerr != nil {
		t.Fatalf("concept missing after the override: %v", gerr)
	}
	fields, ferr := flattenConceptFields(live)
	if ferr != nil {
		t.Fatalf("read the live concept's fields: %v", ferr)
	}
	if _, stillThere := fields["sku"]; stillThere {
		t.Error("the override did not land -- the live concept still declares `sku`")
	}
}

// TestConceptRowCount_NarrowedEnumCountsOnlyTheValuesThatStoppedBeingLegal:
// counting every row of the concept would overstate a narrowed status enum by
// orders of magnitude and push an operator toward an override they did not need.
func TestConceptRowCount_NarrowedEnumCountsOnlyTheValuesThatStoppedBeingLegal(t *testing.T) {
	eng, ctx := conceptDiffDBEngine(t)

	ns := "enumtest" + strings.ToLower(strings.ReplaceAll(id.NewShortId(), "-", ""))[:12]
	conceptId := "v1:" + ns + ":ticket"

	v1 := fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("A ticket")
concept ticket {
  ownerUserId  string  @required
  status       enum("open", "closed", "archived")
}`, ns)
	mutationSrc := fmt.Sprintf(`use %s.concepts.{ ticket }

@actor
@description("Create a ticket")
mutate ticket createTicket%s {
  args {
    ticketId  string  @required
    status    string  @required
  }
  insert {
    id:          args.ticketId
    ownerUserId: actor.userId
    status:      args.status
  }
}`, ns, ns)

	promoteOrFail(t, eng, ctx, v1)
	promoteOrFail(t, eng, ctx, mutationSrc)

	// Five rows, of which exactly two hold the value that is about to become
	// illegal. A count of 5 would be "every row"; a count of 2 is the answer.
	seed := []string{"open", "open", "open", "archived", "archived"}
	for _, status := range seed {
		call := fmt.Sprintf(`mutation createTicket%s(ticketId: %q, status: %q)`, ns, ns+"-t-"+id.NewShortId(), status)
		if _, err := eng.Execute(ctx, call); err != nil {
			t.Fatalf("seed a %q ticket: %v", status, err)
		}
	}

	v2 := fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("A ticket")
concept ticket {
  ownerUserId  string  @required
  status       enum("open", "closed")
}`, ns)

	res, err := eng.PromoteBundleDurable(ctx, "owner-1", v2, "", false)
	if err == nil {
		t.Fatal("narrowing an enum away from a value rows still hold was allowed")
	}
	if len(res.ConceptDiffs) != 1 {
		t.Fatalf("refusal reply carries %d diffs, want 1", len(res.ConceptDiffs))
	}
	change := changeFor(t, res.ConceptDiffs[0], ConceptChangeEnumNarrowed)
	if !change.RowCountKnown {
		t.Fatal("the narrowed-enum refusal reports the row count as unknown against a live database")
	}
	if change.RowsAffected != 2 {
		t.Errorf("narrowed-enum count = %d, want 2 (the rows holding \"archived\"), not %d (every row of %s)",
			change.RowsAffected, len(seed), conceptId)
	}
	if !strings.Contains(change.Detail, `"archived"`) {
		t.Errorf("the refusal does not name the value that stopped being legal: %q", change.Detail)
	}
}

// conceptDiffListHas is a local membership check over the "kind:name" reference
// list a classified change carries.
func conceptDiffListHas(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
