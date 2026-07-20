package sense

import "testing"

// #2625: unknown actor members squiggle at edit time, mirroring the
// load-time gate.
func TestActorUnknownPropertyRule(t *testing.T) {
	src := "@actor\nquery todo todos {\n  filter todo.ownerUserId == actor.displayName\n}\n"
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
		"valid-canonical": "@actor\nquery todo todos {\n  filter todo.ownerUserId == actor.userId\n}\n",
		"valid-alias":     "@actor\nquery todo todos {\n  filter todo.done == actor.isOwner\n}\n",
		"valid-now":       "@actor\nquery todo todos {\n  filter todo.dueAt < actor.now\n}\n",
		"prose-rank":      "// governance compares actor.rank to target.rank\nquery todo todos {\n  filter todo.done == false\n}\n",
		"string-prose":    "@description(\"reads actor.displayName internally\")\nquery todo todos {\n  filter todo.done == false\n}\n",
		"event-stamp":     "@trigger(event=\"x.y\")\nautomation onThing {\n  step run {\n    who := event.actor.id\n  }\n}\n",
	} {
		if got := actorUnknownPropertyRule(clean); len(got) != 0 {
			t.Errorf("%s: want no diagnostics, got %+v", name, got)
		}
	}

	// Second occurrence anchors on ITS line, not the first.
	repeated := "@actor\nquery a x {\n  filter x.o == actor.userId\n}\n\n@actor\nquery b y {\n  filter y.o == actor.nope\n}\n"
	got = actorUnknownPropertyRule(repeated)
	if len(got) != 1 || got[0].Range.Start.Line != 8 {
		t.Fatalf("want one diagnostic on line 8, got %+v", got)
	}
}
