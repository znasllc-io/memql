package sense

import "testing"

// #2625: unknown actor members squiggle at edit time, mirroring the
// load-time gate.
func TestActorUnknownPropertyRule(t *testing.T) {
	src := "@actor\nquery todo todos {\n  filter row => row.ownerUserId == actor.displayName\n}\n"
	got := actorUnknownPropertyRule(src)
	if len(got) != 1 || got[0].Code != "actor-unknown-property" || got[0].Severity != SeverityError {
		t.Fatalf("want one actor-unknown-property Error, got %+v", got)
	}
	if got[0].Range.Start.Line != 3 {
		t.Errorf("anchor line = %d, want 3", got[0].Range.Start.Line)
	}
	if got[0].Range.End.Column-got[0].Range.Start.Column != len("displayName") {
		t.Errorf("range must underline the member token: %+v", got[0].Range)
	}

	for name, clean := range map[string]string{
		"valid-canonical": "@actor\nquery todo todos {\n  filter row => row.ownerUserId == actor.userId\n}\n",
		"valid-alias":     "@actor\nquery todo todos {\n  filter row => row.done == actor.isOwner\n}\n",
		"valid-now":       "@actor\nquery todo todos {\n  filter row => row.dueAt < actor.now\n}\n",
		"prose-rank":      "// governance compares actor.rank to target.rank\nquery todo todos {\n  filter row => row.done == false\n}\n",
		"string-prose":    "@description(\"reads actor.displayName internally\")\nquery todo todos {\n  filter row => row.done == false\n}\n",
		"event-stamp":     "logic onThing {\n  args {\n    event object!\n  }\n  return args.event.actor.id\n}\n",
	} {
		if got := actorUnknownPropertyRule(clean); len(got) != 0 {
			t.Errorf("%s: want no diagnostics, got %+v", name, got)
		}
	}

	// Second occurrence anchors on ITS line, not the first.
	repeated := "@actor\nquery a x {\n  filter row => row.x.o == actor.userId\n}\n\n@actor\nquery b y {\n  filter row => row.y.o == actor.nope\n}\n"
	got = actorUnknownPropertyRule(repeated)
	if len(got) != 1 || got[0].Range.Start.Line != 8 {
		t.Fatalf("want one diagnostic on line 8, got %+v", got)
	}
}
