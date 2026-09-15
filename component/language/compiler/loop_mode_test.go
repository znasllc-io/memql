package compiler

import (
	"reflect"
	"testing"
)

// TestLoopAndModeCompile: @loop compiles to {maxDepth, until} with until as
// canonical v1 source, and @mode to {kind, max}. Two flags are carried
// together, so the load refuses them by the names written (mode_flags), and
// max appears only when it was written: a compiled 0 would read as the
// default.
func TestLoopAndModeCompile(t *testing.T) {
	compile := func(head string) map[string]any {
		t.Helper()
		res, err := CompileSource(`@trigger(event="system.startup")
` + head + `
automation probeLoopMode {
  step run {
    mutation createThing (id: "x")
  }
}`)
		if err != nil {
			t.Fatalf("CompileSource(%s): %v", head, err)
		}
		if len(res.Automations) != 1 {
			t.Fatalf("compiled %d automations", len(res.Automations))
		}
		return res.Automations[0].JSON
	}

	out := compile(`@filter(row => row.status != "done")
@loop(until=row => (row.status == "done"), maxDepth=3)
@mode(queued, max=7)`)
	if want := map[string]any{"maxDepth": 3, "until": `row => (row.status == "done")`}; !reflect.DeepEqual(out["loop"], want) {
		t.Errorf("loop = %#v, want %#v", out["loop"], want)
	}
	if want := map[string]any{"kind": "queued", "max": 7}; !reflect.DeepEqual(out["mode"], want) {
		t.Errorf("mode = %#v, want %#v", out["mode"], want)
	}

	out = compile(`@mode(single, queued)`)
	if want := map[string]any{"kind": "single,queued"}; !reflect.DeepEqual(out["mode"], want) {
		t.Errorf("two flags: mode = %#v, want %#v", out["mode"], want)
	}
	if _, ok := out["loop"]; ok {
		t.Errorf("an automation with no @loop compiled a loop: %#v", out["loop"])
	}

	out = compile(`@description("No loop and no mode.")`)
	for _, key := range []string{"loop", "mode"} {
		if _, ok := out[key]; ok {
			t.Errorf("an automation with neither annotation compiled %s: %#v", key, out[key])
		}
	}
}
