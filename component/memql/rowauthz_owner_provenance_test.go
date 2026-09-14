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
			IDTemplate: v1Expr(t, `args.thingId`),
			// A bare expression, not a map: the whole-object splat.
			PayloadTemplate: v1Expr(t, `args.payload`),
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
//
// SYNTHETIC, and it did not used to be -- same story as
// TestSplatWithoutOverlayIsCallerWritable above. This was
// `appendDocumentVersion` verbatim until memql#2989 fixed it; the tree
// no longer contains the shape, so the fixture is built by hand rather
// than asserted against a mutation that is now correct. The live pair is
// preserved in TestLibraryOwnerFieldsAreServerStamped (the fix).
//
// The value is a parsed node, not the source text: that is the form the
// loader produces for a bare `args.X` line, and memql#2840's trap was
// precisely an analyzer that classified the printed form instead.
func TestBareArgsMirrorIsCallerWritable(t *testing.T) {
	reg := newFunctionRegistry()
	if err := reg.Upsert(&Function{
		Name:         "appendThingVersionUnsafe",
		FunctionKind: "mutation",
		BoundConcept: "v1:probe:thingVersion",
		MutationTemplate: &FunctionMutationTemplate{
			Concept:    "v1:probe:thingVersion",
			IDTemplate: v1Expr(t, `args.versionId`),
			// A longhand insert: an object literal with a bare
			// `args.ownerUserId` mirror and no accept block anywhere.
			PayloadTemplate: map[string]any{
				"thingId":     v1Expr(t, `args.thingId`),
				"ownerUserId": v1Expr(t, `args.ownerUserId`),
			},
			PayloadOverlayTemplate: map[string]any{},
		},
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	got := provenanceOf(t, reg, "v1:probe:thingVersion", "ownerUserId")
	if got.ServerStamped {
		t.Fatalf("a bare args.ownerUserId mirror in a longhand insert reported server-stamped. "+
			"There is no accept block to scan, which is exactly why this analyzer reads the "+
			"loaded template instead. StampedBy=%v", got.StampedBy)
	}
	if len(got.WritableBy) == 0 {
		t.Fatal("no mutation was named as caller-writable, so the diagnostic is unusable")
	}
}

// The live half of memql#2989, asserted against the REAL tree: both
// library concepts that once declared an owner tier over a
// caller-supplied field now stamp it from actor.userId.
//
// This is the regression test for the fix, and it is the reason
// ownerGateExemptions is empty. It fails against the pre-fix tree in
// three distinct ways -- `createGeneratedOutput` and
// `updateGeneratedOutputContent` accepted the field, and
// `appendDocumentVersion` mirrored it -- so a partial revert of any one
// of the three is caught here by name rather than only as a count.
func TestLibraryOwnerFieldsAreServerStamped(t *testing.T) {
	reg := loadTreeRegistry(t)

	for _, tc := range []struct {
		concept string
		stamps  []string
	}{
		{
			concept: "v1:library:generatedOutput",
			stamps:  []string{"createGeneratedOutput", "updateGeneratedOutputContent"},
		},
		{
			concept: "v1:library:documentVersion",
			stamps:  []string{"appendDocumentVersion"},
		},
	} {
		got := provenanceOf(t, reg, tc.concept, "ownerUserId")
		if !got.ServerStamped {
			t.Errorf("%s.ownerUserId reported caller-writable (%s); memql#2989 moved it into a "+
				"stamp. WritableBy=%v", tc.concept, got.Reason, got.WritableBy)
			continue
		}
		if len(got.WritableBy) != 0 {
			t.Errorf("%s.ownerUserId: WritableBy=%v, want empty", tc.concept, got.WritableBy)
		}
		// Name every stamping mutation, not just the count: a mutation
		// dropping its stamp while a sibling keeps one is the partial
		// revert this test exists to catch, and clause 2 of the analyzer
		// is a universal quantifier only over WRITES -- a mutation that
		// stops writing the field at all would still leave ServerStamped
		// true on the strength of its sibling.
		for _, want := range tc.stamps {
			found := false
			for _, name := range got.StampedBy {
				if name == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s.ownerUserId: %s is not among StampedBy=%v -- it must stamp the owner "+
					"from actor.userId", tc.concept, want, got.StampedBy)
			}
		}
	}
}

// The three library mutations must not take an `ownerUserId` argument at
// all. Dropping it from `accept { }` while leaving it declared in
// `args { }` would leave a caller-supplied value that silently does
// nothing -- an arg the SDK still generates, the docs still describe,
// and a caller reasonably believes sets the owner.
//
// Provenance cannot see this: a declared-but-unwritten arg is not a
// write, so OwnerFieldProvenance would report the concept clean.
func TestLibraryOwnerMutationsTakeNoOwnerArg(t *testing.T) {
	reg := loadTreeRegistry(t)

	for _, name := range []string{
		"createGeneratedOutput",
		"updateGeneratedOutputContent",
		"appendDocumentVersion",
	} {
		fn, err := reg.Get(name)
		if err != nil {
			t.Errorf("%s not loaded: %v", name, err)
			continue
		}
		if fn.ArgsSchema == nil {
			t.Errorf("%s has no args schema, so this assertion cannot see its arguments", name)
			continue
		}
		for _, f := range fn.ArgsSchema.Fields {
			if f != nil && f.Name == "ownerUserId" {
				t.Errorf("%s still declares an ownerUserId arg. The field is stamped from "+
					"actor.userId (memql#2989), so a caller-supplied one is a value that looks "+
					"like it sets the owner and does not.", name)
			}
		}
	}
}

// The accept-block shape, which is the one the issue originally
// specified and the only one a source scan would have caught.
//
// THE SUBJECT MOVED, AND THE OLD ONE MADE THIS TEST VACUOUS (memql#5053).
// It named `v1:planner:plan.requestedBy` until that concept was deleted, and
// `OwnerFieldProvenance` answers a concept it has never heard of with a zero
// verdict whose `ServerStamped` is false and whose Reason is "no mutation
// writes this concept". So the assertion below passed on a concept that did
// not exist -- the whole test, green, asserting nothing.
//
// `v1:library:artifact.ownerUserId` is the live subject now: `createArtifact`
// writes it from `args.ownerUserId`, and the tree-wide gate carries it as a
// TRACKED exemption (memql#4340 / #2803) rather than an oversight. A
// synthetic fixture would not do here -- an `accept { }` block lowers into
// exactly the `field: args.field` PayloadTemplate entry a longhand line
// produces, so a hand-built one would be byte-identical to
// TestBareArgsMirrorIsCallerWritable's and this test's only remaining
// distinction is that its subject is REAL.
//
// The WritableBy assertion is what makes it non-vacuous, and it is the line
// that was missing: a concept nobody writes has an EMPTY WritableBy, so this
// can no longer pass by naming nothing. It also fails if the exemption is
// ever paid off, which is correct -- the tree-wide gate errors on a stale
// exemption for the same reason, and both should be repaired together.
func TestAcceptedOwnerFieldIsCallerWritable(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:library:artifact", "ownerUserId")
	if len(got.WritableBy) == 0 {
		t.Fatalf("no mutation writes library.artifact.ownerUserId (%s). This test asserts that a "+
			"caller-accepted owner field is REPORTED caller-writable, so a subject nothing "+
			"writes makes it vacuous -- which is exactly how it survived the deletion of its "+
			"previous subject. Re-point it at a live accepted-owner field, or make it "+
			"synthetic and say so.", got.Reason)
	}
	if got.ServerStamped {
		t.Fatalf("library.artifact.ownerUserId reported server-stamped; createArtifact writes it "+
			"from args.ownerUserId. StampedBy=%v writableBy=%v. If the exemption "+
			"(memql#4340) has been paid off, TestDeclaredOwnerFieldsAreServerStamped will be "+
			"failing on the stale entry too -- fix both.", got.StampedBy, got.WritableBy)
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
// today, so a whole-value test happens to give the right answer
// everywhere. It is one authored `args.X ?? actor.userId` away from
// silently passing a forgeable field, which is exactly the shape an
// author reaches for when one mutation must serve two call paths.
func TestClassifierFoldsCompoundExpressionsToCallerControlled(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want valueProvenance
	}{
		{"pure stamp", v1Expr(t, `actor.userId`), provStamp},
		{"pure accept", v1Expr(t, `args.ownerUserId`), provAccept},
		{"literal", v1Expr(t, `"predefined"`), provNone},
		{"coalesce, caller first", v1Expr(t, `args.ownerUserId ?? actor.userId`), provAccept},
		{"coalesce, actor first", v1Expr(t, `actor.userId ?? args.ownerUserId`), provAccept},
		{"nested map with a caller leaf", map[string]any{"a": v1Expr(t, `actor.userId`), "b": v1Expr(t, `args.x`)}, provAccept},
		{"nested map all stamped", map[string]any{"a": v1Expr(t, `actor.userId`)}, provStamp},
		{"array with a caller leaf", []any{v1Expr(t, `now`), v1Expr(t, `args.x`)}, provAccept},
		// A quoted literal is a literal: a description string mentioning
		// "args." cannot flip a verdict.
		{"prose mentioning args inside a literal", v1Expr(t, `"set from args.userId by the handler"`), provNone},
		// The retired ctx root reads nothing this analyzer can place.
		{"a name it cannot place", v1Expr(t, `ctx.ownerUserId`), provUnknown},
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

// isPayloadSplat separates an object literal from a whole-object splat.
func TestIsPayloadSplat(t *testing.T) {
	if isPayloadSplat(map[string]any{"ownerUserId": v1Expr(t, `actor.userId`)}) {
		t.Fatal("an object literal is not a splat")
	}
	if !isPayloadSplat(v1Expr(t, `args.payload`)) {
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
// Every template value is a parsed node now, and memql#2840 was an actor
// reference landing in exactly such a slot: the node is read, never its
// print.
func TestLoweredAstNodesAreClassifiedNotRenderedToText(t *testing.T) {
	caller := v1Expr(t, `args.ownerUserId`)
	if got := classifyTemplateValue(caller); got != provAccept {
		t.Fatalf("classifyTemplateValue(args.ownerUserId) = %v, want provAccept. "+
			"Sprintf renders it as %q, which is why text classification failed open here.",
			got, fmtValue(caller))
	}
	actor := v1Expr(t, `actor.userId`)
	if got := classifyTemplateValue(actor); got != provStamp {
		t.Fatalf("classifyTemplateValue(actor.userId) = %v, want provStamp (memql#2840)", got)
	}
	// A node of the internal query form has no place in a template, and one
	// that reached it anyway -- the ArgRefExpr the retired evaluator read
	// both roots through -- is not guessed at.
	if got := classifyTemplateValue(&langparser.ArgRefExpr{Path: "actor.userId"}); got != provUnknown {
		t.Fatalf("classifyTemplateValue(*ArgRefExpr) = %v, want provUnknown: a value this analyzer "+
			"does not read must fail closed", got)
	}
}

// An unrecognised value must fail CLOSED. This is clause 5, and it was
// unreachable while the default arm rendered to text: every input
// produced a non-empty string and landed on provNone. Text itself is one
// of them now -- a template holds parsed nodes, and a string in one is a
// value nothing built.
func TestUnrecognisedValueFailsClosed(t *testing.T) {
	type unknownShape struct{ X int }
	for _, v := range []any{unknownShape{1}, &unknownShape{2}, uint(3), float32(4), "args.ownerUserId", "actor.userId"} {
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
				PayloadTemplate: map[string]any{"ownerUserId": v1Expr(t, `actor.userId`)},
			},
		},
		{
			Name: "zzForgeThing", FunctionKind: "mutation", BoundConcept: "v1:probe:thing",
			MutationTemplate: &FunctionMutationTemplate{
				Concept: "v1:probe:thing",
				PayloadTemplate: map[string]any{
					"ownerUserId": v1Expr(t, `args.ownerUserId`),
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
		t.Fatal("a caller reference in a parsed node was invisible, and a stamping sibling " +
			"carried the concept to a pass. That is the fail-open shape this analyzer shipped with.")
	}
	if len(got.WritableBy) != 1 || got.WritableBy[0] != "zzForgeThing" {
		t.Fatalf("writableBy = %v, want exactly [zzForgeThing] so the diagnostic names the "+
			"mutation to fix", got.WritableBy)
	}
}

func fmtValue(v any) string { return strings.TrimSpace(fmt.Sprintf("%v", v)) }
