package memql

import (
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// TestNoDuplicateConstructs is the load-time uniqueness gate for the
// embedded core tree (epic #2351 / S5, memql#2360): no two authored
// constructs that land in the same runtime registry may share a name.
// The product pack's cross-repo half of this gate lives in the carrier
// repo's pack tree-load test (it boots engine + pack).
func TestNoDuplicateConstructs(t *testing.T) {
	dups := DetectDuplicateConstructs(baseloader.ReadAll(nil))
	if len(dups) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("duplicate construct name(s) in the embedded DSL tree ")
	b.WriteString("(same registry, silent last-wins registration):\n")
	for _, d := range dups {
		b.WriteString("  " + d.String() + "\n")
	}
	b.WriteString("Rename or delete the duplicate. Constructs that share a " +
		"group share ONE name-keyed registry; a collision silently drops " +
		"one definition at load.")
	t.Fatal(b.String())
}

// TestDetectDuplicateConstructs_Fixture exercises the detector on synthetic
// sources: it must (a) catch a genuine collision -- the same name twice in the
// SAME namespace, (b) NOT flag the same name in two DIFFERENT namespaces, which
// memql#3897 made legal, (c) NOT flag an automation and a same-named logic (they
// land in different registries), and (d) NOT flag a nested logic
// step-invocation inside an automation body as a top-level logic declaration.
//
// (b) is the one that changed, and it is the whole point of memql#3897. The
// gate refused a duplicate TREE-WIDE, which was right while the twelve flat
// kinds shared one registry keyed by the bare name -- the second declaration
// really did overwrite the first. Now the key is `<namespace>.<name>`, so both
// coexist and each resolves to its own. Refusing that IS the constraint the
// issue exists to remove: it is what stopped a product DSL bundle declaring a
// construct whose name the engine had already used.
func TestDetectDuplicateConstructs_Fixture(t *testing.T) {
	fileA := baseloader.RawFile{
		Path: "alpha/shapes.memql",
		Content: `
@row
shape widget widgetFull {
  row.id
  name
}
`,
	}
	fileB := baseloader.RawFile{
		Path: "beta/shapes.memql",
		Content: `
@row
shape widget widgetFull {
  row.id
  label
}
`,
	}
	// An automation that both declares the automation name AND invokes a
	// same-named logic as a nested step. The logic itself is a top-level
	// declaration in a separate file.
	fileC := baseloader.RawFile{
		Path: "gamma/automations.memql",
		Content: `
@trigger(event="node.created", concept="v1:gamma:thing", partition="*")
automation syncThing {
  step run {
    logic syncThing { event: event }
  }
}
`,
	}
	fileD := baseloader.RawFile{
		Path: "gamma/logic.memql",
		Content: `
logic syncThing {
  args { event object @required }
  body { return noop({}) }
}
`,
	}

	// The SAME name twice in the SAME namespace -- still a real collision,
	// because both land on one key and the second silently wins.
	fileE := baseloader.RawFile{
		Path: "alpha/moreShapes.memql",
		Content: `
@row
shape widget alphaOnly {
  row.id
}
`,
	}
	fileF := baseloader.RawFile{
		Path: "alpha/evenMoreShapes.memql",
		Content: `
@row
shape widget alphaOnly {
  name
}
`,
	}

	dups := DetectDuplicateConstructs([]baseloader.RawFile{fileA, fileB, fileC, fileD, fileE, fileF})

	// widgetFull is declared in alpha AND beta -- two namespaces, so NOT a
	// duplicate. This is memql#3897's first acceptance bullet.
	for _, d := range dups {
		if d.Name == "widgetFull" {
			t.Fatalf("two namespaces declaring the same flat construct must NOT be a "+
				"collision -- the registry is keyed <namespace>.<name> and each resolves to "+
				"its own (memql#3897). Refusing this is the constraint that stopped a product "+
				"DSL bundle naming a construct the engine had already named: %v", d)
		}
	}

	// alphaOnly is declared twice in ALPHA -- one key, silent last-wins, still
	// refused.
	var same *DuplicateConstruct
	for i := range dups {
		if dups[i].Name == "alphaOnly" {
			same = &dups[i]
		}
	}
	if same == nil {
		t.Fatalf("the same name twice in ONE namespace is still a silent last-wins "+
			"overwrite and must still be caught, got %v", dups)
	}
	if same.Group != "shapes" || same.Namespace != "alpha" {
		t.Fatalf("expected [shapes] alphaOnly in namespace alpha, got [%s] %s in %q",
			same.Group, same.Name, same.Namespace)
	}
	if len(same.Origins) != 2 {
		t.Fatalf("expected 2 origins, got %d: %v", len(same.Origins), same.Origins)
	}
	sort.Strings(same.Origins)
	if !strings.Contains(same.Origins[0], "alpha/evenMoreShapes.memql") ||
		!strings.Contains(same.Origins[1], "alpha/moreShapes.memql") {
		t.Fatalf("origins should name both alpha files, got %v", same.Origins)
	}

	// The automation `syncThing` and the top-level logic `syncThing` land
	// in different registries -> not a collision. And the nested `logic
	// syncThing` step invocation inside the automation body must NOT be
	// counted as a second top-level logic declaration.
	for _, d := range dups {
		if d.Name == "syncThing" {
			t.Fatalf("automation/logic same-name pair must NOT be flagged: %v", d)
		}
	}
}
