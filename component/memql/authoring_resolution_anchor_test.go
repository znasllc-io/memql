package memql

import (
	"strings"
	"testing"
)

// authoring_resolution_anchor_test.go -- memql#3801.
//
// THE DEFECT. Every authoring diagnostic raised BEFORE the per-construct parse
// landed at line 0, which the client honestly renders as "no position" and
// stacks on line 1 of the buffer:
//
//	"message": "query allPlans: concept resolution: signature concept \"plan\":
//	            ambiguous concept name \"plan\" matches 2 concepts ...
//	            (the engine reported no source position for this failure)",
//	"fileLevel": true,
//	"start": { "line": 0, "character": 0 }
//
// Nine constructs failed in that bundle. Nine underlines on the first line, and
// the construct that actually failed identifiable only by reading constructName
// out of the message text.
//
// WHY IT HAPPENED, and it is not the obvious reason. resolveAuthoredPosition
// needs a languageParser.ParseError to map through the lowering. A resolution
// failure has none -- it is raised before the parse that would produce one --
// so the position stayed zero and #2375's "zero means absent" contract was
// honoured exactly as written.
//
// The contract says: never emit a WRONG line. It was being applied as: never
// emit a line you cannot establish PRECISELY. Those differ. The failure is
// already attributed to a named construct, and SplitBundleSource already knows
// where that construct's body begins in the bundle -- located by strings.Index
// on a verbatim body, not by a heuristic. So the construct's signature line is
// the coarsest TRUE position, not the finest false one.
//
// These tests pin all three acceptance criteria, including the third, which is
// the one that keeps the contract intact: a failure attributable to no
// construct still reports zero.

// anchorBundle is a two-construct bundle whose constructs PARSE cleanly and
// fail afterwards -- which is the class this issue is about. Both queries read
// `actor.*` without declaring `@actor`, a semantic check raised with no
// languageParser.ParseError to map, exactly like concept resolution.
//
// The fixture was chosen by RUNNING it, not by reasoning about it: an earlier
// draft bound a concept the registry does not define and compiled cleanly, so
// the end-to-end test measured nothing. That test is what caught it -- the unit
// tests over constructAnchor passed against a bundle that produced no failures
// at all.
const anchorBundle = `@namespace("alpha")
concept widget {
  ownerUserId string
}

@description("first")
query widget allA {
  filter  ownerUserId==actor.userId
  shape   widgetFull
}

@description("second")
query widget allB {
  filter  ownerUserId==actor.userId
  shape   widgetFull
}
`

// TestConstructAnchorPointsAtTheSignatureNotTheAnnotation is the core of it.
//
// The body a slice captures begins at the `@description` line, so anchoring at
// BundleLine alone would underline the annotation. The author is looking for
// `query plan allAllPlans {`.
func TestConstructAnchorPointsAtTheSignatureNotTheAnnotation(t *testing.T) {
	constructs := SplitBundleSource(anchorBundle)
	if len(constructs) < 2 {
		t.Fatalf("splitter produced %d constructs, want >=2 -- the fixture is not "+
			"exercising a multi-construct bundle:\n%s", len(constructs), anchorBundle)
	}

	lines := strings.Split(anchorBundle, "\n")
	for _, c := range constructs {
		pos := constructAnchor(c)
		if pos.Line <= 0 {
			t.Errorf("%s %q: anchor line = %d, want > 0. A construct the splitter located "+
				"verbatim has a knowable position; emitting zero is what stacked every "+
				"diagnostic on line 1 (memql#3801).", c.Kind, c.Name, pos.Line)
			continue
		}
		got := strings.TrimSpace(lines[pos.Line-1])
		// The signature line, not the annotation above it and not a body line.
		if !strings.Contains(got, c.Name) || !(strings.HasPrefix(got, "query ") || strings.HasPrefix(got, "concept ")) {
			t.Errorf("%s %q: anchor line %d is %q, want that construct's signature line.\n"+
				"Anchoring at the body's first line would land on @description, which is "+
				"not what the author is looking for.", c.Kind, c.Name, pos.Line, got)
		}
	}
}

// TestSeveralConstructsProduceDistinctAnchors is acceptance criterion two, and
// it is the one the reported symptom is actually about. Nine failures stacking
// on one line is indistinguishable from one failure; distinct positions are the
// whole benefit.
func TestSeveralConstructsProduceDistinctAnchors(t *testing.T) {
	constructs := SplitBundleSource(anchorBundle)

	seen := map[int]string{}
	for _, c := range constructs {
		pos := constructAnchor(c)
		if pos.Line <= 0 {
			continue
		}
		if prev, dup := seen[pos.Line]; dup {
			t.Errorf("%s %q and %s both anchor to line %d. Distinct constructs must get "+
				"distinct positions -- nine diagnostics on one line is the symptom "+
				"memql#3801 reports.", c.Kind, c.Name, prev, pos.Line)
		}
		seen[pos.Line] = c.Kind + " " + c.Name
	}
	if len(seen) < 2 {
		t.Fatalf("only %d distinct anchor(s) were produced, so this test did not measure "+
			"what it claims", len(seen))
	}
}

// TestAnchorIsZeroWithoutAVerbatimBundleAnchor is acceptance criterion three,
// and it is the one that preserves #2375.
//
// A construct whose body the splitter could NOT locate verbatim in the bundle
// has BundleLine == 0. There is no true position to report, so nothing is
// reported -- exactly as before. That is also the path a failure attributable
// to no construct at all takes, since it carries no construct.
func TestAnchorIsZeroWithoutAVerbatimBundleAnchor(t *testing.T) {
	c := SandboxConstruct{
		Name:       "orphan",
		Kind:       "query",
		Source:     "query plan orphan {\n  filter isActiveRecord\n}\n",
		BundleLine: 0, // the splitter could not find this body in the bundle
	}
	if pos := constructAnchor(c); pos.Line != 0 {
		t.Errorf("anchor line = %d, want 0. Without a verbatim bundle anchor there is no "+
			"true position, and #2375's contract is that the sandbox emits none rather "+
			"than a guess. memql#3801 narrows WHEN a position is knowable, not whether "+
			"an unknowable one may be invented.", pos.Line)
	}
}

// TestSignatureOffsetFallsBackInsideTheConstruct pins the failure mode of the
// scan itself.
//
// signatureLineOffset looks for the kind's keyword. If a kind's signature is
// spelled some way the scan does not recognise, it returns 0 -- the body's
// first line -- rather than a line elsewhere. Both answers are inside the
// construct's own span, so the fallback is COARSER, never wrong, which is the
// property that lets this run on the failure path of code that did not compile.
func TestSignatureOffsetFallsBackInsideTheConstruct(t *testing.T) {
	c := SandboxConstruct{
		Name:   "x",
		Kind:   "prompt", // not a kind signatureKeywords knows
		Source: "@description(\"a\")\nprompt x {\n  field string\n}\n",
	}
	if got := signatureLineOffset(c); got != 0 {
		t.Errorf("offset = %d, want 0 for an unrecognised kind -- the fallback must be the "+
			"body's first line, which is inside the construct, rather than a scan result "+
			"from somewhere else", got)
	}
}

// TestMutationSignatureUsesTheMutateKeyword covers the one kind whose name and
// keyword differ. A `mutation` is declared `mutate NAME`, so a scan keyed on
// the kind string alone would miss every mutation in the tree and silently
// anchor them all at their annotation line.
func TestMutationSignatureUsesTheMutateKeyword(t *testing.T) {
	c := SandboxConstruct{
		Name:   "createThing",
		Kind:   "mutation",
		Source: "@description(\"c\")\nmutate thing createThing {\n  insert { id: args.id }\n}\n",
	}
	if got := signatureLineOffset(c); got != 1 {
		t.Errorf("offset = %d, want 1 (the `mutate` line, not the @description above it). "+
			"`mutation` is the one kind whose keyword differs from its name.", got)
	}
}

// TestBundleDiagnosticsCarryDistinctPositions is the acceptance criterion end
// to end, through the public entry point rather than the anchor helper.
//
// The unit tests above prove constructAnchor computes the right line. This
// proves the line REACHES the diagnostic -- which is the thing that was broken,
// and which a correct helper wired to nothing would not fix.
func TestBundleDiagnosticsCarryDistinctPositions(t *testing.T) {
	rep := SandboxCompileBundle(SplitBundleSource(anchorBundle))

	var failed []SandboxDiagnostic
	for _, d := range rep.Diagnostics {
		if !d.OK && !d.Skipped {
			failed = append(failed, d)
		}
	}
	if len(failed) < 2 {
		t.Fatalf("the fixture produced %d failing diagnostic(s), want >=2 -- this test "+
			"cannot measure 'several failures get several positions' without several "+
			"failures.\ndiagnostics: %+v", len(failed), rep.Diagnostics)
	}

	seen := map[int]string{}
	for _, d := range failed {
		if d.Line <= 0 {
			t.Errorf("%s %q failed at line %d with no position: %s\n"+
				"This is memql#3801: the client renders a zero line as fileLevel and "+
				"stacks every such diagnostic on line 1, so the failing construct is "+
				"identifiable only by reading its name out of the message.",
				d.Kind, d.Name, d.Line, d.Error)
			continue
		}
		if prev, dup := seen[d.Line]; dup {
			t.Errorf("%s %q and %s both report line %d -- distinct constructs must get "+
				"distinct positions", d.Kind, d.Name, prev, d.Line)
		}
		seen[d.Line] = d.Kind + " " + d.Name
	}
}
