package memql

import (
	"fmt"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// loadTreeRegistry loads the real DSL tree once for the fixture tests.
// These assert against the tree rather than synthetic templates on
// purpose: every one of them was a live case, and a synthetic fixture
// would have let the analyzer pass a shape the tree actually contains.
func loadTreeRegistry(t *testing.T) *FunctionRegistry {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	reg := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, reg, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	return reg
}

func provenanceOf(t *testing.T, reg *FunctionRegistry, concept, field string) OwnerProvenance {
	t.Helper()
	got := OwnerFieldProvenance(reg, map[string]string{concept: field})
	if len(got) != 1 {
		t.Fatalf("OwnerFieldProvenance(%s.%s) returned %d verdicts, want 1", concept, field, len(got))
	}
	return got[0]
}

// CLAUSE 3 -- the case no source-level check sees (memql#2988).
//
// A mutation that splats a caller-supplied payload without re-stamping
// the owner field leaves it caller-writable. memql#401's overlay-wins
// protection is populated ONLY from explicit block fields, so with no
// overlay entry the caller's value lands in the row unchallenged.
//
// SYNTHETIC, and it did not used to be. This was `updateCalendarEvent`
// verbatim -- `update { id: args.eventId; args.payload }` on a concept
// declaring `@rowAuthz(owner="ownerUserId")` -- until this change fixed
// it. The tree no longer contains the shape, so the fixture is built by
// hand rather than asserted against a mutation that is now correct.
// The real pair is preserved in TestUpdateCalendarEventReStampsTheOwner
// (the fix) and in the safe-side assertion below.
func TestSplatWithoutOverlayIsCallerWritable(t *testing.T) {
	reg := newFunctionRegistry()
	if err := reg.Upsert(&Function{
		Name:         "updateThingUnsafe",
		FunctionKind: "mutation",
		BoundConcept: "v1:probe:thing",
		MutationTemplate: &FunctionMutationTemplate{
			Concept:    "v1:probe:thing",
			IDTemplate: "args.thingId",
			// A bare expression, not a map: the whole-object splat.
			PayloadTemplate: "args.payload",
			// EMPTY -- this is the defect.
			PayloadOverlayTemplate: map[string]any{},
		},
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	got := provenanceOf(t, reg, "v1:probe:thing", "ownerUserId")
	if got.ServerStamped {
		t.Fatal("a mutation splatting a caller payload with no owner overlay reported " +
			"server-stamped. That is memql#2988: the caller sets the field directly and " +
			"memql#401's overlay-wins protection never engages, because it only covers explicit " +
			"block fields.")
	}
	if !strings.Contains(got.Reason, "splat") {
		t.Fatalf("reason = %q, want it to name the splat as the mechanism -- the diagnostic is "+
			"what tells an author the create-time stamp does not carry over", got.Reason)
	}

	// The safe counterpart, asserted against the REAL tree: an overlay
	// entry re-stamping the field is what makes the identical source
	// shape safe.
	treeReg := loadTreeRegistry(t)
	safe := provenanceOf(t, treeReg, "v1:notes:note", "ownerUserId")
	if !safe.ServerStamped {
		t.Fatalf("notes.note.ownerUserId reported caller-writable (%s) -- updateNote re-stamps it "+
			"in its overlay, so this is the analyzer being wrong, not the tree", safe.Reason)
	}
}

// The bare-mirror shape: a longhand insert writing `args.ownerUserId`
// as a plain field, with no accept block anywhere in the mutation. The
// issue's originally-specified "scan the accept block" check is blind
// to this.
func TestBareArgsMirrorIsCallerWritable(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:library:documentVersion", "ownerUserId")
	if got.ServerStamped {
		t.Fatalf("library.documentVersion.ownerUserId reported server-stamped; "+
			"appendDocumentVersion writes a bare args.ownerUserId mirror. StampedBy=%v", got.StampedBy)
	}
	if len(got.WritableBy) == 0 {
		t.Fatal("no mutation was named as caller-writable, so the diagnostic is unusable")
	}
}

// The accept-block shape, which is the one the issue originally
// specified and the only one a source scan would have caught.
func TestAcceptedOwnerFieldIsCallerWritable(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:planner:plan", "requestedBy")
	if got.ServerStamped {
		t.Fatal("planner.plan.requestedBy reported server-stamped; createPlan accepts it from " +
			"caller args. This is the fixture memql#2982 was filed around.")
	}
}

// Clause 2 is a universal quantifier, not an existential. A field one
// mutation stamps and another accepts is caller-writable: the generic
// write reopens what the purpose-built one protects.
func TestStampedByOneAcceptedByAnotherIsCallerWritable(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:forge:request", "validatedByUserId")
	if got.ServerStamped {
		t.Fatalf("forge.request.validatedByUserId reported server-stamped. validateRequest stamps "+
			"it, but the generic advanceRequest accepts it -- 'some mutation stamps it' is not "+
			"sufficient. StampedBy=%v WritableBy=%v", got.StampedBy, got.WritableBy)
	}
	if len(got.StampedBy) == 0 {
		t.Fatal("expected validateRequest to be recorded as stamping it, so the report can show " +
			"both halves of the conflict")
	}
}

// A field genuinely stamped by every write path passes. Without this
// the gate could be satisfied by an analyzer that rejects everything.
func TestGenuinelyStampedOwnerFieldPasses(t *testing.T) {
	reg := loadTreeRegistry(t)
	for _, tc := range []struct{ concept, field string }{
		{"v1:notes:note", "ownerUserId"},
		{"v1:todos:todo", "ownerUserId"},
	} {
		got := provenanceOf(t, reg, tc.concept, tc.field)
		if !got.ServerStamped {
			t.Errorf("%s.%s reported caller-writable: %s (writableBy=%v)",
				tc.concept, tc.field, got.Reason, got.WritableBy)
		}
	}
}

// ---- classifier unit tests (clause 5, and the fold rules) ----

// Clause 5, forward-looking: the tree has no compound owner write
// today, so a whole-string prefix test happens to give the right answer
// everywhere. It is one authored `args.X ?? actor.userId` away from
// silently passing a forgeable field, which is exactly the shape an
// author reaches for when one mutation must serve two call paths.
func TestClassifierFoldsCompoundExpressionsToCallerControlled(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want valueProvenance
	}{
		{"pure stamp", "actor.userId", provStamp},
		{"pure accept", "args.ownerUserId", provAccept},
		{"ctx alias", "ctx.ownerUserId", provAccept},
		{"literal", `"predefined"`, provNone},
		{"coalesce, caller first", "args.ownerUserId ?? actor.userId", provAccept},
		{"coalesce, actor first", "actor.userId ?? args.ownerUserId", provAccept},
		{"nested map with a caller leaf", map[string]any{"a": "actor.userId", "b": "args.x"}, provAccept},
		{"nested map all stamped", map[string]any{"a": "actor.userId"}, provStamp},
		{"array with a caller leaf", []any{"now", "args.x"}, provAccept},
		{"prose mentioning args inside a literal", `"set from args.userId by the handler"`, provNone},
		{"nil", nil, provNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTemplateValue(tc.v); got != tc.want {
				t.Fatalf("classifyTemplateValue(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// A quoted literal must not be read as a reference, or a description
// string silently flips a verdict.
func TestStripQuotedLiteralsPreservesLength(t *testing.T) {
	for _, s := range []string{
		`actor.userId`,
		`"args.x"`,
		`a ?? "quoted \" args.y" ?? actor.userId`,
		`"unterminated args.z`,
	} {
		if got := stripQuotedLiterals(s); len(got) != len(s) {
			t.Fatalf("stripQuotedLiterals(%q) changed length %d -> %d", s, len(s), len(got))
		}
	}
}

// isPayloadSplat separates an object literal from a whole-object splat.
func TestIsPayloadSplat(t *testing.T) {
	if isPayloadSplat(map[string]any{"ownerUserId": "actor.userId"}) {
		t.Fatal("an object literal is not a splat")
	}
	if !isPayloadSplat("args.payload") {
		t.Fatal("a bare args.payload expression is a splat")
	}
	if isPayloadSplat(nil) {
		t.Fatal("no payload is not a splat")
	}
}

// The analyzer must not silently pass a concept with no write path --
// nothing forges the field, but nothing stamps it either.
func TestConceptWithNoMutationsIsReportedNotPassed(t *testing.T) {
	got := OwnerFieldProvenance(newFunctionRegistry(), map[string]string{"v1:nope:missing": "ownerUserId"})
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(got))
	}
	if got[0].ServerStamped {
		t.Fatal("a concept with no mutation reported server-stamped")
	}
	if got[0].Reason == "" {
		t.Fatal("a failing verdict must carry a reason")
	}
}

// Guard against the parser's kind spelling drifting under us: if
// FunctionKind stops being "mutation", every verdict silently becomes
// "no mutation writes this concept" and the gate passes vacuously.
func TestMutationKindSpellingIsWhatWeFilterOn(t *testing.T) {
	reg := loadTreeRegistry(t)
	n := 0
	for _, fn := range reg.List() {
		if fn != nil && fn.FunctionKind == "mutation" && fn.MutationTemplate != nil {
			n++
		}
	}
	if n == 0 {
		t.Fatal(`no loaded function has FunctionKind == "mutation" with a template. Either the ` +
			`tree failed to load or the kind spelling changed -- OwnerFieldProvenance filters on ` +
			`it, so every verdict would silently become "no mutation writes this concept".`)
	}
	_ = langparser.RowAuthzOwned // keep the tier vocabulary in view for readers
}

// The memql#2988 regression, stated as behaviour rather than as a
// provenance verdict: a caller-supplied payload carrying a foreign
// ownerUserId must not displace the actor-stamped value.
//
// This asserts the OVERLAY exists and is actor-derived, which is what
// memql#401's overlay-wins precedence consumes at render time. It is
// the property the one-line fix restored, and it fails if someone
// removes `ownerUserId: actor.userId` from updateCalendarEvent on the
// reasoning that create already stamps it.
func TestUpdateCalendarEventReStampsTheOwner(t *testing.T) {
	reg := loadTreeRegistry(t)
	fn, err := reg.Get("updateCalendarEvent")
	if err != nil {
		t.Fatalf("updateCalendarEvent not loaded: %v", err)
	}
	if fn.MutationTemplate == nil {
		t.Fatal("updateCalendarEvent has no mutation template")
	}
	overlay, ok := fn.MutationTemplate.PayloadOverlayTemplate["ownerUserId"]
	if !ok {
		t.Fatal("updateCalendarEvent has no ownerUserId overlay. It splats args.payload, so " +
			"without an overlay entry a caller can set ownerUserId directly and reassign the row " +
			"-- memql#2988. memql#401's overlay-wins protection is populated ONLY from explicit " +
			"block fields, so the explicit `ownerUserId: actor.userId` line is load-bearing and " +
			"is NOT redundant with the create-time stamp.")
	}
	if got := classifyTemplateValue(overlay); got != provStamp {
		t.Fatalf("updateCalendarEvent's ownerUserId overlay is %v, want actor-derived -- an "+
			"overlay that reads from caller args reopens memql#2988 while looking fixed", got)
	}
}

// THE FAIL-OPEN THIS ANALYZER SHIPPED WITH, AND MUST NOT AGAIN.
//
// An earlier version's default arm rendered unrecognised values with
// fmt.Sprintf("%v") and classified the text. A lowered
// *ArgRefExpr{Path:"ownerUserId"} prints as `&{ownerUserId}` -- which
// contains neither "args." nor "actor.userId" -- so a value that IS a
// caller reference classified as "mentions neither", contributed to
// neither StampedBy nor WritableBy, and let a sibling stamping mutation
// carry the concept to a PASS.
//
// Not hypothetical: IDTemplate is a lowered node for most mutations
// today, evalValue has an explicit ExpressionNode arm, and memql#2840
// was an actor reference landing in exactly such a slot.
func TestLoweredAstNodesAreClassifiedNotRenderedToText(t *testing.T) {
	caller := &langparser.ArgRefExpr{Path: "ownerUserId"}
	if got := classifyTemplateValue(caller); got != provAccept {
		t.Fatalf("classifyTemplateValue(*ArgRefExpr{ownerUserId}) = %v, want provAccept. "+
			"Sprintf renders it as %q, which is why text classification failed open here.",
			got, fmtValue(caller))
	}
	actor := &langparser.ArgRefExpr{Path: "actor.userId"}
	if got := classifyTemplateValue(actor); got != provStamp {
		t.Fatalf("classifyTemplateValue(*ArgRefExpr{actor.userId}) = %v, want provStamp -- the "+
			"parser routes BOTH args.X and actor.X through ArgRefExpr, so the prefix is the only "+
			"discriminator (memql#2840)", got)
	}
}

// An unrecognised value must fail CLOSED. This is clause 5, and it was
// unreachable while the default arm rendered to text: every input
// produced a non-empty string and landed on provNone.
func TestUnrecognisedValueFailsClosed(t *testing.T) {
	type unknownShape struct{ X int }
	for _, v := range []any{unknownShape{1}, &unknownShape{2}, uint(3), float32(4)} {
		if got := classifyTemplateValue(v); got != provUnknown {
			t.Fatalf("classifyTemplateValue(%#v) = %v, want provUnknown. Anything this analyzer "+
				"cannot classify must fail closed -- treating it as 'no reference' is how a "+
				"forgeable field passes.", v, got)
		}
	}
}

// End to end: a forging mutation whose value is an AST node, beside a
// stamping sibling. Under the fail-open version the concept PASSED.
func TestAstNodeForgeryIsVisibleBesideAStampingSibling(t *testing.T) {
	reg := newFunctionRegistry()
	for _, fn := range []*Function{
		{
			Name: "zzStampThing", FunctionKind: "mutation", BoundConcept: "v1:probe:thing",
			MutationTemplate: &FunctionMutationTemplate{
				Concept:         "v1:probe:thing",
				PayloadTemplate: map[string]any{"ownerUserId": "actor.userId"},
			},
		},
		{
			Name: "zzForgeThing", FunctionKind: "mutation", BoundConcept: "v1:probe:thing",
			MutationTemplate: &FunctionMutationTemplate{
				Concept: "v1:probe:thing",
				PayloadTemplate: map[string]any{
					"ownerUserId": &langparser.ArgRefExpr{Path: "ownerUserId"},
				},
			},
		},
	} {
		if err := reg.Upsert(fn); err != nil {
			t.Fatalf("seed %s: %v", fn.Name, err)
		}
	}
	got := provenanceOf(t, reg, "v1:probe:thing", "ownerUserId")
	if got.ServerStamped {
		t.Fatal("a caller reference in a lowered AST node was invisible, and a stamping sibling " +
			"carried the concept to a pass. That is the fail-open shape this analyzer shipped with.")
	}
	if len(got.WritableBy) != 1 || got.WritableBy[0] != "zzForgeThing" {
		t.Fatalf("writableBy = %v, want exactly [zzForgeThing] so the diagnostic names the "+
			"mutation to fix", got.WritableBy)
	}
}

func fmtValue(v any) string { return strings.TrimSpace(fmt.Sprintf("%v", v)) }
