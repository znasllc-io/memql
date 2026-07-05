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

// TestDetectDuplicateConstructs_Fixture exercises the detector on
// synthetic sources: it must (a) catch a genuine same-registry
// collision, (b) NOT flag an automation and a same-named logic (they
// land in different registries), and (c) NOT flag a nested logic
// step-invocation inside an automation body as a top-level logic
// declaration.
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

	dups := DetectDuplicateConstructs([]baseloader.RawFile{fileA, fileB, fileC, fileD})

	// Exactly one duplicate: the widgetFull shape across two files.
	if len(dups) != 1 {
		t.Fatalf("expected exactly 1 duplicate (widgetFull shape), got %d: %v", len(dups), dups)
	}
	got := dups[0]
	if got.Group != "shapes" || got.Name != "widgetFull" {
		t.Fatalf("expected [shapes] widgetFull, got [%s] %s", got.Group, got.Name)
	}
	if len(got.Origins) != 2 {
		t.Fatalf("expected 2 origins, got %d: %v", len(got.Origins), got.Origins)
	}
	sort.Strings(got.Origins)
	if !strings.Contains(got.Origins[0], "alpha/shapes.memql") ||
		!strings.Contains(got.Origins[1], "beta/shapes.memql") {
		t.Fatalf("origins should name both shape files, got %v", got.Origins)
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
