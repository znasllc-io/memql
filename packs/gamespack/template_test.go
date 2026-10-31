package gamespack

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func torchPlan(count float64) []PlanStep {
	return []PlanStep{
		{Action: "collect", Args: map[string]any{"block": "coal", "count": float64(3)}},
		{Action: "craft", Args: map[string]any{"item": "torch", "count": count}},
		{Action: "placeTorches", Args: map[string]any{"count": count}},
	}
}

func TestGeneralizeKeepsAgreeingArgumentsAndOpensDifferingOnes(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tmpl.Holes, []string{"1.count", "2.count"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("holes = %v, want %v", got, want)
	}
	if spec := tmpl.Steps[0].Args["block"]; spec.Hole != "" || spec.Const != "coal" {
		t.Fatalf("collect.block = %+v, want the constant coal", spec)
	}
	if spec := tmpl.Steps[0].Args["count"]; spec.Hole != "" || spec.Const != float64(3) {
		t.Fatalf("collect.count = %+v, want the constant 3", spec)
	}
	if spec := tmpl.Steps[2].Args["count"]; spec.Hole != "2.count" {
		t.Fatalf("placeTorches.count = %+v, want the hole 2.count", spec)
	}
}

func TestGeneralizeRefusesPlansThatDisagreeOnShape(t *testing.T) {
	if _, err := Generalize([][]PlanStep{torchPlan(4)}); err == nil {
		t.Fatal("one plan is a record, not a pattern; generalizing it must be refused")
	}
	other := torchPlan(6)
	other[2].Action = "lookAt"
	if _, err := Generalize([][]PlanStep{torchPlan(4), other}); err == nil {
		t.Fatal("plans with different action sequences must not generalize")
	}
	extra := torchPlan(6)
	extra[1].Args["near"] = "home"
	if _, err := Generalize([][]PlanStep{torchPlan(4), extra}); err == nil {
		t.Fatal("a step whose argument keys differ must not generalize")
	}
}

func TestBindReadsHoleValuesAndRefusesADifferentConstant(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := Bind(tmpl, torchPlan(8))
	if !ok {
		t.Fatal("a plan of the same shape must bind")
	}
	if binding["1.count"] != float64(8) || binding["2.count"] != float64(8) {
		t.Fatalf("binding = %v", binding)
	}
	wrong := torchPlan(8)
	wrong[0].Args["block"] = "iron_ore"
	if _, ok := Bind(tmpl, wrong); ok {
		t.Fatal("a plan whose constant differs is not an instance of the skill")
	}
	if _, ok := Bind(tmpl, torchPlan(8)[:2]); ok {
		t.Fatal("a shorter plan is not an instance of the skill")
	}
}

func TestInstantiateFillsHolesAndRefusesAnUnfilledOne(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Instantiate(tmpl, map[string]any{"1.count": float64(5), "2.count": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, torchPlan(5)) {
		t.Fatalf("plan = %+v, want %+v", plan, torchPlan(5))
	}
	if _, err := Instantiate(tmpl, map[string]any{"1.count": float64(5)}); err == nil || !strings.Contains(err.Error(), "2.count") {
		t.Fatalf("an unfilled hole must be refused by name, got %v", err)
	}
}

func TestTemplateSurvivesItsStoredForm(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stored)
	if !strings.Contains(string(raw), `"hole":"2.count"`) || !strings.Contains(string(raw), `"const":"coal"`) {
		t.Fatalf("stored form = %s", raw)
	}
	back, err := templateFrom(stored)
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest() != tmpl.Digest() {
		t.Fatal("the digest must not change across the stored form, or no promotion could ever be decided")
	}
}

func TestEqualityIsJSONEquality(t *testing.T) {
	a := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": 4}}}
	b := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": float64(4)}}}
	tmpl, err := Generalize([][]PlanStep{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpl.Holes) != 0 {
		t.Fatalf("4 and 4.0 are one value; holes = %v", tmpl.Holes)
	}
	c := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": "4"}}}
	if tmpl2, _ := Generalize([][]PlanStep{a, c}); len(tmpl2.Holes) != 1 {
		t.Fatalf(`4 and "4" are two values; holes = %v`, tmpl2.Holes)
	}
}

func TestSkillIDIsDeterministicPerOwnerSignatureActionsAndGeneration(t *testing.T) {
	a := SkillID("u1", "sig", []string{"collect", "craft"}, 1)
	if a != SkillID("u1", "sig", []string{"collect", "craft"}, 1) || !strings.HasPrefix(a, "gs-") || len(a) != 27 {
		t.Fatalf("SkillID = %q", a)
	}
	if a != SkillID("v1:identity:user:u1", "sig", []string{"collect", "craft"}, 1) {
		t.Fatal("a bare and a canonical spelling of one owner must name one skill")
	}
	if a == SkillID("u2", "sig", []string{"collect", "craft"}, 1) {
		t.Fatal("two owners teaching one goal must get two skills; one row would merge their ladders")
	}
	if a == SkillID("u1", "sig", []string{"craft", "collect"}, 1) {
		t.Fatal("a different action order is a different skill")
	}
	if a == SkillID("u1", "sig", []string{"collect", "craft"}, 2) {
		t.Fatal("a second generation is a different skill")
	}
}

// The tests below pin what the contract above leaves open: what Bind refuses,
// the order of holes, every plan being compared, the stored form and digest,
// the key a skill id is hashed from, and the plan reader's refusals.

func TestGeneralizeRefusesArgumentsNamedDifferentlyAtTheSameCount(t *testing.T) {
	a := []PlanStep{{Action: "craft", Args: map[string]any{"item": "torch", "count": float64(4)}}}
	b := []PlanStep{{Action: "craft", Args: map[string]any{"item": "torch", "number": float64(4)}}}
	if _, err := Generalize([][]PlanStep{a, b}); err == nil {
		t.Fatal("steps with as many arguments but different names must not generalize")
	}
	if _, err := Generalize([][]PlanStep{b, a}); err == nil {
		t.Fatal("the order of the plans must not matter to that refusal")
	}
}

func TestGeneralizeComparesEveryPlanNotJustTheFirstTwo(t *testing.T) {
	otherAction := torchPlan(4)
	otherAction[2].Action = "lookAt"
	if _, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(4), otherAction}); err == nil {
		t.Fatal("a third plan with a different action must not generalize")
	}
	extra := torchPlan(4)
	extra[1].Args["near"] = "home"
	if _, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(4), extra}); err == nil {
		t.Fatal("a third plan with different argument keys must not generalize")
	}
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.count", "2.count"}; !reflect.DeepEqual(tmpl.Holes, want) {
		t.Fatalf("holes = %v, want %v: a value that differs only in the third plan is still a hole", tmpl.Holes, want)
	}
}

// Holes come in step order, then argument-key order, every time: the digest is
// over the stored form, and a promotion approves exactly one digest.
func TestGeneralizeOrdersHolesByStepThenArgumentKey(t *testing.T) {
	plan := func(n float64) []PlanStep {
		return []PlanStep{
			{Action: "build", Args: map[string]any{"x": n, "y": n, "z": n, "width": n, "depth": n, "height": n}},
			{Action: "stay", Args: map[string]any{"seconds": n}},
		}
	}
	want := []string{"0.depth", "0.height", "0.width", "0.x", "0.y", "0.z", "1.seconds"}
	var digest string
	for i := 0; i < 20; i++ { // a map is ranged in a different order each time
		tmpl, err := Generalize([][]PlanStep{plan(1), plan(2)})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tmpl.Holes, want) {
			t.Fatalf("holes = %v, want %v", tmpl.Holes, want)
		}
		stored, err := tmpl.AsMap()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := templateFrom(stored); err != nil {
			t.Fatalf("a template Generalize made must read back, so the reader and Generalize must order holes alike: %v", err)
		}
		if i == 0 {
			digest = tmpl.Digest()
		} else if tmpl.Digest() != digest {
			t.Fatal("one set of plans gave two digests")
		}
	}
}

func TestGeneralizeRefusesPlansOfDifferentLengthsWithoutPanicking(t *testing.T) {
	for name, plans := range map[string][][]PlanStep{
		"a shorter second plan": {torchPlan(4), torchPlan(4)[:2]},
		"a longer second plan":  {torchPlan(4)[:2], torchPlan(4)},
		"an empty second plan":  {torchPlan(4), {}},
		"an empty first plan":   {{}, torchPlan(4)},
		"a nil second plan":     {torchPlan(4), nil},
	} {
		if _, err := Generalize(plans); err == nil {
			t.Errorf("%s: plans of different lengths must not generalize", name)
		}
	}
}

func TestBindRefusesPlansThatAreNotInstances(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	longer := append(torchPlan(8), PlanStep{Action: "stay", Args: map[string]any{}})
	otherAction := torchPlan(8)
	otherAction[1].Action = "smelt"
	extraArg := torchPlan(8)
	extraArg[2].Args["near"] = "home"
	renamedArg := torchPlan(8)
	delete(renamedArg[2].Args, "count")
	renamedArg[2].Args["number"] = float64(8)
	for name, plan := range map[string][]PlanStep{
		"a longer plan":                  longer,
		"a step with another action":     otherAction,
		"a step with an extra argument":  extraArg,
		"an argument under another name": renamedArg,
	} {
		if binding, ok := Bind(tmpl, plan); ok {
			t.Errorf("%s bound as %v, but it is not an instance of the skill", name, binding)
		}
	}
}

func TestDigestIsTheTemplatesAndNotTheTracesThatTaughtIt(t *testing.T) {
	a, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	same, err := Generalize([][]PlanStep{torchPlan(1), torchPlan(9), torchPlan(2)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != same.Digest() {
		t.Fatal("plans of one shape teach one template, and a template has one digest")
	}
	iron := func(count float64) []PlanStep {
		plan := torchPlan(count)
		plan[0].Args["block"] = "iron_ore"
		return plan
	}
	other, err := Generalize([][]PlanStep{iron(4), iron(6)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() == other.Digest() {
		t.Fatal("a template with another constant is another version, and must have another digest")
	}
}

// The stored form and the digest are persisted (a promotion approves one
// digest), so both are pinned. The digest literal was hashed outside the code:
// a change here orphans every stored approval.
func TestTemplateStoredFormAndDigestArePinned(t *testing.T) {
	tmpl := Template{
		Steps: []TemplateStep{
			{Action: "craft", Args: map[string]ArgSpec{"count": {Hole: "0.count"}, "item": {Const: "torch"}}},
			{Action: "stay", Args: map[string]ArgSpec{}},
		},
		Holes: []string{"0.count"},
	}
	const wantJSON = `{"steps":[{"action":"craft","args":{"count":{"hole":"0.count"},"item":{"const":"torch"}}},{"action":"stay","args":{}}],"holes":["0.count"]}`
	if got := canonicalJSON(tmpl); got != wantJSON {
		t.Fatalf("canonical form = %s\nwant             %s", got, wantJSON)
	}
	if got, want := tmpl.Digest(), "b0f63676581a04c4394bc113947d7dd559af5dce74f50335f3bf2e4b84e7679c"; got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}

func TestTemplateOfNoHolesStoresAnEmptyHoleList(t *testing.T) {
	plan := []PlanStep{{Action: "stay", Args: map[string]any{"seconds": float64(4)}}}
	tmpl, err := Generalize([][]PlanStep{plan, plan})
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Holes == nil {
		t.Fatal("Generalize returns an empty list of holes, not nil, so that even json.Marshal of the template writes []")
	}
	stored, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	if holes, ok := stored["holes"].([]any); !ok || len(holes) != 0 {
		t.Fatalf("stored holes = %#v, want an empty list", stored["holes"])
	}
}

func TestTemplateKeepsNullAndStructuredConstantsThroughItsStoredForm(t *testing.T) {
	plan := func(n float64) []PlanStep {
		return []PlanStep{{Action: "go", Args: map[string]any{
			"target": nil,
			"path":   []any{"a", float64(1)},
			"opts":   map[string]any{"fast": true},
			"n":      n,
		}}}
	}
	tmpl, err := Generalize([][]PlanStep{plan(1), plan(2)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0.n"}; !reflect.DeepEqual(tmpl.Holes, want) {
		t.Fatalf("holes = %v, want %v: a null, a list and an object that agree are constants", tmpl.Holes, want)
	}
	stored, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	back, err := templateFrom(stored)
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest() != tmpl.Digest() {
		t.Fatal("the digest must survive the stored form when constants are null or structured")
	}
	binding, ok := Bind(back, plan(3))
	if !ok || !reflect.DeepEqual(binding, map[string]any{"0.n": float64(3)}) {
		t.Fatalf("Bind = %v, %v", binding, ok)
	}
	got, err := Instantiate(back, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, plan(3)) {
		t.Fatalf("plan = %+v, want %+v", got, plan(3))
	}
}

func TestTemplateFromRefusesAnArgumentThatCannotBeBound(t *testing.T) {
	stored := func(arg any) any {
		return map[string]any{
			"steps": []any{map[string]any{"action": "stay", "args": map[string]any{"seconds": arg}}},
			"holes": []any{},
		}
	}
	const prefix = "games: the stored template does not read back: "
	for _, tc := range []struct {
		name   string
		stored any
		want   string
	}{
		{"an argument that is neither a hole nor a const", stored(map[string]any{"value": 4}), "neither a hole nor a const"},
		{"a hole and a const", stored(map[string]any{"hole": "0.seconds", "const": 4}), "exactly one key"},
		{"a const and an extra key", stored(map[string]any{"const": 4, "note": "x"}), "exactly one key"},
		{"a hole and an extra key", stored(map[string]any{"hole": "0.seconds", "note": "x"}), "exactly one key"},
		{"an empty object", stored(map[string]any{}), "exactly one key"},
		{"null", stored(nil), "exactly one key"},
		{"text", stored("4"), "exactly one key"},
		{"a list", stored([]any{"hole", "0.seconds"}), "exactly one key"},
		{"an empty hole", stored(map[string]any{"hole": ""}), "must be a non-empty string"},
		{"a null hole", stored(map[string]any{"hole": nil}), "must be a non-empty string"},
		{"a hole that is not text", stored(map[string]any{"hole": 5}), "must be a non-empty string"},
		{"steps that are not a list", map[string]any{"steps": "stay"}, "cannot unmarshal string"},
	} {
		_, err := templateFrom(tc.stored)
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal starting %q and containing %q", tc.name, err, prefix, tc.want)
		}
	}

	// A const may be any value, null and the empty ones included.
	for _, c := range []any{nil, float64(0), "", false, []any{}, map[string]any{}, "x"} {
		back, err := templateFrom(stored(map[string]any{"const": c}))
		if err != nil {
			t.Errorf("const %#v was refused: %v", c, err)
			continue
		}
		if spec := back.Steps[0].Args["seconds"]; spec.Hole != "" || !reflect.DeepEqual(spec.Const, c) {
			t.Errorf("const %#v read back as %+v", c, spec)
		}
	}
}

// The preimage is ownerKey|signature|actions, with |<generation> appended from
// the second generation on: the first keeps the plain key, so a skill named
// before a second generation existed keeps its id. The literals were hashed
// outside the code.
func TestSkillIDKeyIsPinnedForTheFirstAndLaterGenerations(t *testing.T) {
	actions := []string{"collect", "craft"}
	for gen, want := range map[int]string{
		1: "gs-0daad7fa6ab5003da31a164c",
		2: "gs-82358e39c0d5612e7c3a1289",
		3: "gs-1a391815bec2a9d4c66ad665",
	} {
		if got := SkillID("u1", "sig", actions, gen); got != want {
			t.Errorf("SkillID(u1, sig, %v, %d) = %s, want %s", actions, gen, got, want)
		}
	}
	if SkillID("u1", "sig", actions, 1) == SkillID("u1", "other-sig", actions, 1) {
		t.Fatal("a different goal signature is a different skill")
	}
}

func TestParsePlanReadsStepsAndRefusesWhatIsNotAPlan(t *testing.T) {
	plan, err := parsePlan([]any{
		map[string]any{"action": "  craft ", "args": map[string]any{"item": "torch"}},
		map[string]any{"action": "stay"},
		map[string]any{"action": "look", "args": nil},
		map[string]any{"action": "place torches/2.v1;now"}, // other punctuation is a legal name
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []PlanStep{
		{Action: "craft", Args: map[string]any{"item": "torch"}},
		{Action: "stay", Args: map[string]any{}},
		{Action: "look", Args: map[string]any{}},
		{Action: "place torches/2.v1;now", Args: map[string]any{}},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %+v, want %+v", plan, want)
	}

	// The refusing step is the second, so the index each message names is pinned.
	second := func(step any) []any { return []any{map[string]any{"action": "stay"}, step} }
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"text", "craft", "a plan must be a list"},
		{"nothing", nil, "a plan must be a list"},
		{"an object", map[string]any{"action": "stay"}, "a plan must be a list"},
		{"a step that is not an object", second("craft"), "plan step 1 is not an object"},
		{"a step with no action", second(map[string]any{"args": map[string]any{}}), "plan step 1 names no action"},
		{"a step with a blank action", second(map[string]any{"action": " \t"}), "plan step 1 names no action"},
		{"a step with a null action", second(map[string]any{"action": nil}), "plan step 1 names no action"},
		{"a number for the action", second(map[string]any{"action": float64(5)}), "plan step 1 action must be a string"},
		{"true for the action", second(map[string]any{"action": true}), "plan step 1 action must be a string"},
		{"a list for the action", second(map[string]any{"action": []any{"craft"}}), "plan step 1 action must be a string"},
		{"an object for the action", second(map[string]any{"action": map[string]any{"name": "craft"}}), "plan step 1 action must be a string"},
		{"a comma in the action", second(map[string]any{"action": "craft,smelt"}), `plan step 1 action "craft,smelt" holds a comma or a vertical bar`},
		{"a vertical bar in the action", second(map[string]any{"action": "craft|smelt"}), `plan step 1 action "craft|smelt" holds a comma or a vertical bar`},
		{"a comma left after trimming", second(map[string]any{"action": " craft, "}), `plan step 1 action "craft," holds a comma or a vertical bar`},
		{"args that are a list", second(map[string]any{"action": "x", "args": []any{}}), "plan step 1 args is not an object"},
		{"args that are text", second(map[string]any{"action": "x", "args": "n=1"}), "plan step 1 args is not an object"},
	} {
		got, err := parsePlan(tc.in)
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: parsePlan = %+v, %v; want a games_invalid_argument refusal containing %q", tc.name, got, err, tc.want)
		}
	}
}

// A template read back is checked: the holes it lists are the holes of its steps,
// in the order Generalize records them, each named for its place.
func TestTemplateFromChecksTheHolesItLists(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)}) // the holes are 1.count and 2.count
	if err != nil {
		t.Fatal(err)
	}
	stored := func(edit func(m map[string]any)) any {
		m, err := tmpl.AsMap() // a fresh copy each time
		if err != nil {
			t.Fatal(err)
		}
		edit(m)
		return m
	}
	args := func(m map[string]any, step int) map[string]any {
		return m["steps"].([]any)[step].(map[string]any)["args"].(map[string]any)
	}
	if _, err := templateFrom(stored(func(map[string]any) {})); err != nil {
		t.Fatalf("the template as stored must read back: %v", err)
	}
	const prefix = "games: the stored template does not read back: "
	for _, tc := range []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"a hole left out of the list", func(m map[string]any) { m["holes"] = []any{"1.count"} },
			"lists the holes [1.count] but its steps hold [1.count 2.count]"},
		{"a hole listed that no step holds", func(m map[string]any) { m["holes"] = []any{"1.count", "2.count", "3.count"} },
			"lists the holes [1.count 2.count 3.count] but its steps hold [1.count 2.count]"},
		{"the holes in another order", func(m map[string]any) { m["holes"] = []any{"2.count", "1.count"} },
			"lists the holes [2.count 1.count] but its steps hold [1.count 2.count]"},
		{"a hole listed twice", func(m map[string]any) { m["holes"] = []any{"1.count", "1.count", "2.count"} },
			"lists the holes [1.count 1.count 2.count] but its steps hold [1.count 2.count]"},
		{"no list at all", func(m map[string]any) { delete(m, "holes") },
			"lists the holes [] but its steps hold [1.count 2.count]"},
		{"a null list", func(m map[string]any) { m["holes"] = nil },
			"lists the holes [] but its steps hold [1.count 2.count]"},
		{"a constant turned into a hole nobody lists", func(m map[string]any) { args(m, 0)["block"] = map[string]any{"hole": "0.block"} },
			"lists the holes [1.count 2.count] but its steps hold [0.block 1.count 2.count]"},
		{"a hole not named for its place", func(m map[string]any) {
			args(m, 1)["count"] = map[string]any{"hole": "7.count"}
			m["holes"] = []any{"7.count", "2.count"}
		}, `argument "count" of step 1 is the hole "7.count", want "1.count"`},
		{"one hole id on two arguments", func(m map[string]any) {
			args(m, 0)["block"] = map[string]any{"hole": "1.count"}
			m["holes"] = []any{"1.count", "1.count", "2.count"}
		}, `argument "block" of step 0 is the hole "1.count", want "0.block"`},
	} {
		_, err := templateFrom(stored(tc.edit))
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal starting %q and containing %q", tc.name, err, prefix, tc.want)
		}
	}
}

// A nil list or map is an empty one everywhere a template is read: its digest, its
// stored form, and what templateFrom makes of a store that wrote null or nothing.
func TestTemplateNilListsAndMapsAreTheEmptyOnes(t *testing.T) {
	empty := Template{Steps: []TemplateStep{{Action: "stay", Args: map[string]ArgSpec{}}}, Holes: []string{}}
	nils := Template{Steps: []TemplateStep{{Action: "stay"}}} // nil Args, nil Holes
	if nils.Digest() != empty.Digest() {
		t.Fatal("a nil list or map must count as an empty one in the digest")
	}
	stored, err := nils.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	if nils.Steps[0].Args != nil || nils.Holes != nil {
		t.Fatal("reading a template for its digest or its stored form must not change it")
	}
	if args, ok := stored["steps"].([]any)[0].(map[string]any)["args"].(map[string]any); !ok || len(args) != 0 {
		t.Fatalf("stored args = %#v, want an empty object", stored["steps"].([]any)[0])
	}
	if holes, ok := stored["holes"].([]any); !ok || len(holes) != 0 {
		t.Fatalf("stored holes = %#v, want an empty list", stored["holes"])
	}
	back, err := templateFrom(stored)
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest() != nils.Digest() {
		t.Fatal("the digest must survive the stored form")
	}
	for name, obj := range map[string]any{
		"null":     map[string]any{"steps": []any{map[string]any{"action": "stay", "args": nil}}, "holes": nil},
		"left out": map[string]any{"steps": []any{map[string]any{"action": "stay"}}},
	} {
		got, err := templateFrom(obj)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.Digest() != empty.Digest() || got.Holes == nil || got.Steps[0].Args == nil {
			t.Errorf("%s: read back as %+v, want empty lists and maps", name, got)
		}
	}
	zero, err := templateFrom(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if zero.Steps == nil || zero.Holes == nil || zero.Digest() != (Template{}).Digest() {
		t.Fatalf("an object with nothing in it read back as %+v, want the empty template", zero)
	}
	if got := canonicalJSON(zero); got != `{"steps":[],"holes":[]}` {
		t.Fatalf("the empty template is spelled %s", got)
	}
}

// canonicalJSON's output is persisted: a template's digest is taken over it, and a
// promotion approves that digest. So its spelling is pinned case by case, and a change
// (a new Go release that escapes differently, an encoder option) fails here instead of
// orphaning every stored approval.
func TestCanonicalJSONSpellingIsPinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"no HTML escaping", "a&b<c>", `"a&b<c>"`},
		{"quote and backslash", "say \"hi\" \\ bye", `"say \"hi\" \\ bye"`},
		{"control characters", "a\x01b\tc\nd\be\ff", `"a\u0001b\tc\nd\be\ff"`},
		{"UTF-8 stays as it is", "caf\u00e9 \u0434", "\"caf\u00e9 \u0434\""},
		{"the line separators are escaped", "a\u2028b\u2029c", `"a\u2028b\u2029c"`},
		{"map keys sorted, at any depth", map[string]any{"b": float64(1), "a": []any{true, nil, "x", map[string]any{"z": 1, "y": 2}}}, `{"a":[true,null,"x",{"y":2,"z":1}],"b":1}`},
		{"nil", nil, `null`},
		{"an int, a float64 and an int64 that agree", []any{4, float64(4), int64(4)}, `[4,4,4]`},
		{"a fraction", float64(4.5), `4.5`},
		{"a large float", float64(1e21), `1e+21`},
		{"a small float", float64(1e-7), `1e-7`},
		{"an int64 beyond what a float64 holds", int64(9007199254740993), `9007199254740993`},
		{"a json.Number keeps its spelling", json.Number("4.0"), `4.0`},
	} {
		if got := canonicalJSON(tc.in); got != tc.want {
			t.Errorf("%s: canonicalJSON = %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := canonicalJSON(math.NaN()); !strings.HasPrefix(got, "!") {
		t.Errorf("a value that cannot be encoded answers %q, want a spelling that starts with ! and so is never valid JSON", got)
	}
}

// Inside a template, what ArgSpec.MarshalJSON writes (a constant's value and the id
// inside a hole) is spelled by json.Marshal, which escapes <, > and &, while the action
// name, the argument keys and the list of holes are spelled unescaped. The mix is
// deterministic and part of the persisted digest, so it is pinned. The digest literal
// was hashed outside the code.
func TestTemplateSpellsWhatArgSpecWritesWithJSONMarshalEscaping(t *testing.T) {
	tmpl := Template{
		Steps: []TemplateStep{{Action: "say&go", Args: map[string]ArgSpec{"k&v": {Hole: "0.k&v"}, "text": {Const: "a&b<c>"}}}},
		Holes: []string{"0.k&v"},
	}
	const wantJSON = `{"steps":[{"action":"say&go","args":{"k&v":{"hole":"0.k\u0026v"},"text":{"const":"a\u0026b\u003cc\u003e"}}}],"holes":["0.k&v"]}`
	if got := canonicalJSON(tmpl); got != wantJSON {
		t.Fatalf("canonical form = %s\nwant             %s", got, wantJSON)
	}
	if got, want := tmpl.Digest(), "790e2427cc7a6c274a4995375c13a3c92c5a009d945d08d1df868115a375e68a"; got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
	stored, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	back, err := templateFrom(stored)
	if err != nil {
		t.Fatalf("a template with such names must read back: %v", err)
	}
	if got := back.Steps[0].Args["text"].Const; got != "a&b<c>" || back.Steps[0].Args["k&v"].Hole != "0.k&v" || back.Digest() != tmpl.Digest() {
		t.Fatalf("read back as %#v with digest %s, want the same constant, hole and digest %s", back.Steps[0].Args, back.Digest(), tmpl.Digest())
	}
}

func TestEqualityOfJSONNumbersIsBySpelling(t *testing.T) {
	step := func(v any) []PlanStep { return []PlanStep{{Action: "stay", Args: map[string]any{"seconds": v}}} }
	for _, tc := range []struct {
		name  string
		a, b  any
		holes int
	}{
		{"a Go int and a decoded float64", 4, float64(4), 0},
		{"an int64 and a float64", int64(4), float64(4), 0},
		{"a json.Number spelled like the integer", json.Number("4"), 4, 0},
		{"a json.Number spelled 4.0 is not 4", json.Number("4.0"), 4, 1},
		{"nor is it the float64 4", json.Number("4.0"), float64(4), 1},
		{"two json.Numbers with one spelling", json.Number("4.0"), json.Number("4.0"), 0},
		{"two json.Numbers with two spellings of one value", json.Number("4.0"), json.Number("4"), 1},
	} {
		tmpl, err := Generalize([][]PlanStep{step(tc.a), step(tc.b)})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(tmpl.Holes) != tc.holes {
			t.Errorf("%s: holes = %v, want %d", tc.name, tmpl.Holes, tc.holes)
		}
	}
}

func TestInstantiateNeedsExactlyTheHoles(t *testing.T) {
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)}) // the holes are 1.count and 2.count
	if err != nil {
		t.Fatal(err)
	}
	full := func() map[string]any { return map[string]any{"1.count": float64(5), "2.count": float64(5)} }
	with := func(k string, v any) map[string]any {
		b := full()
		b[k] = v
		return b
	}
	if _, err := Instantiate(tmpl, full()); err != nil {
		t.Fatalf("a binding of exactly the holes must instantiate: %v", err)
	}
	for _, tc := range []struct {
		name    string
		binding map[string]any
		want    string
	}{
		{"a hole left out", map[string]any{"1.count": float64(5)}, "does not fill hole 2.count"},
		{"no binding at all", nil, "does not fill hole 1.count"},
		{"a hole given null", with("2.count", nil), "gives hole 2.count no value (null)"},
		{"a key that names no hole", with("9.count", float64(1)), `names "9.count", which is no hole of the template`},
		{"the place of a constant named as a hole", with("0.block", "coal"), `names "0.block", which is no hole of the template`},
		{"the first unknown key in order is the one named", map[string]any{"1.count": float64(5), "2.count": float64(5), "zz": 1, "yy": 2, "xx": 3, "aa": 4, "bb": 5}, `names "aa"`},
	} {
		for i := 0; i < 10; i++ { // the same answer every time, whatever order a map is ranged in
			got, err := Instantiate(tmpl, tc.binding)
			if err == nil || got != nil || !strings.HasPrefix(err.Error(), "games: ") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: Instantiate = %+v, %v; want a games: refusal containing %q", tc.name, got, err, tc.want)
				break
			}
		}
	}

	// With several holes missing, the one named is the first in step and argument-key
	// order, every time (a map is ranged in a different order each time).
	wide := func(n float64) []PlanStep {
		return []PlanStep{{Action: "build", Args: map[string]any{"x": n, "y": n, "z": n, "width": n, "depth": n, "height": n}}}
	}
	many, err := Generalize([][]PlanStep{wide(1), wide(2)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := Instantiate(many, nil); err == nil || !strings.Contains(err.Error(), "hole 0.depth") {
			t.Fatalf("Instantiate with no binding = %v, want the error to name hole 0.depth", err)
		}
	}
}

// The plan Instantiate returns shares nothing with the template: a caller may change
// it freely. (Its hole values are the binding's own and are not copied.)
func TestInstantiateSharesNothingWithTheTemplate(t *testing.T) {
	plan := func(n float64) []PlanStep {
		return []PlanStep{{Action: "go", Args: map[string]any{
			"path":  []any{"a", map[string]any{"deep": []any{float64(1)}}},
			"opts":  map[string]any{"fast": true, "tags": []any{"x"}},
			"names": []string{"p", "q"}, // a typed slice is copied through its JSON form
			"n":     n,
		}}}
	}
	tmpl, err := Generalize([][]PlanStep{plan(1), plan(2)})
	if err != nil {
		t.Fatal(err)
	}
	before := tmpl.Digest()
	binding := map[string]any{"0.n": float64(3)}
	got, err := Instantiate(tmpl, binding)
	if err != nil {
		t.Fatal(err)
	}

	// Scribble over every nested value of the plan, and drop an argument.
	args := got[0].Args
	args["path"].([]any)[0] = "changed"
	args["path"].([]any)[1].(map[string]any)["deep"].([]any)[0] = float64(99)
	args["opts"].(map[string]any)["fast"] = false
	args["opts"].(map[string]any)["tags"].([]any)[0] = "changed"
	args["opts"].(map[string]any)["added"] = true
	args["names"].([]any)[0] = "changed"
	delete(args, "opts")

	if tmpl.Digest() != before {
		t.Fatal("changing an instantiated plan changed the template")
	}
	again, err := Instantiate(tmpl, binding)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"path":  []any{"a", map[string]any{"deep": []any{float64(1)}}},
		"opts":  map[string]any{"fast": true, "tags": []any{"x"}},
		"names": []any{"p", "q"},
		"n":     float64(3),
	}
	if !reflect.DeepEqual(again[0].Args, want) {
		t.Fatalf("instantiating again gave %#v, want %#v", again[0].Args, want)
	}
}

// The template owns its constants: changing a plan after Generalize does not change it.
func TestGeneralizeCopiesItsConstants(t *testing.T) {
	plan := func(n float64) []PlanStep {
		return []PlanStep{{Action: "go", Args: map[string]any{
			"opts":  map[string]any{"tags": []any{"x"}},
			"names": []string{"p", "q"},
			"n":     n,
		}}}
	}
	a, b := plan(1), plan(2)
	tmpl, err := Generalize([][]PlanStep{a, b})
	if err != nil {
		t.Fatal(err)
	}
	before := tmpl.Digest()
	a[0].Args["opts"].(map[string]any)["tags"].([]any)[0] = "changed"
	a[0].Args["opts"].(map[string]any)["added"] = true
	a[0].Args["names"].([]string)[0] = "changed"
	if tmpl.Digest() != before {
		t.Fatal("changing the first plan after Generalize changed the template")
	}
}

func TestGeneralizeRefusesAConstantItCannotCopy(t *testing.T) {
	plan := func(n float64) []PlanStep {
		return []PlanStep{{Action: "go", Args: map[string]any{"channel": make(chan int), "n": n}}}
	}
	_, err := Generalize([][]PlanStep{plan(1), plan(2)})
	if err == nil || !strings.HasPrefix(err.Error(), "games: ") || !strings.Contains(err.Error(), `argument "channel" of step 0 cannot be copied into a template`) {
		t.Fatalf("err = %v, want a games: refusal naming the argument", err)
	}
}

func TestCloneValueSharesNothingAndKeepsShape(t *testing.T) {
	nested := map[string]any{"list": []any{map[string]any{"k": "v"}}, "n": int64(7), "z": nil}
	c, err := cloneValue(nested)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, nested) {
		t.Fatalf("copy = %#v, want %#v", c, nested)
	}
	c.(map[string]any)["list"].([]any)[0].(map[string]any)["k"] = "changed"
	if nested["list"].([]any)[0].(map[string]any)["k"] != "v" {
		t.Fatal("changing the copy changed the original")
	}
	if got, _ := cloneValue(int64(9007199254740993)); got != int64(9007199254740993) {
		t.Fatalf("an int64 came back as %#v", got)
	}
	if got, _ := cloneValue(map[string]any(nil)); got.(map[string]any) != nil {
		t.Fatalf("a nil map came back as %#v, want it to stay nil", got)
	}
	if got, _ := cloneValue([]any(nil)); got.([]any) != nil {
		t.Fatalf("a nil list came back as %#v, want it to stay nil", got)
	}
	if got, _ := cloneValue([]string{"a", "b"}); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("a typed slice came back as %#v, want its JSON form", got)
	}
	if _, err := cloneValue(make(chan int)); err == nil {
		t.Fatal("a value that cannot be encoded cannot be copied")
	}
}

// Whatever Bind accepts, Instantiate must accept, and Instantiate refuses a hole with
// no value (null). So a plan with a null at a hole is not an instance of the skill,
// while a null that is a CONSTANT, the same in every plan, is part of the shape and
// still binds.
func TestBindRefusesANullAtAHoleButBindsANullConstant(t *testing.T) {
	plan := func(n any) []PlanStep {
		return []PlanStep{{Action: "go", Args: map[string]any{"target": nil, "n": n}}}
	}
	tmpl, err := Generalize([][]PlanStep{plan(float64(1)), plan(float64(2))}) // target is a null constant, n is a hole
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0.n"}; !reflect.DeepEqual(tmpl.Holes, want) {
		t.Fatalf("holes = %v, want %v", tmpl.Holes, want)
	}
	if binding, ok := Bind(tmpl, plan(nil)); ok {
		t.Fatalf("a plan with null at hole 0.n bound as %v, but Instantiate refuses a null", binding)
	}
	binding, ok := Bind(tmpl, plan(float64(3)))
	if !ok || !reflect.DeepEqual(binding, map[string]any{"0.n": float64(3)}) {
		t.Fatalf("a plan with a value at the hole and the null constant must bind: %v, %v", binding, ok)
	}

	// A skill learned from a trace that had null at the hole still refuses that null.
	learned, err := Generalize([][]PlanStep{plan(nil), plan(float64(2))})
	if err != nil {
		t.Fatal(err)
	}
	if binding, ok := Bind(learned, plan(nil)); ok {
		t.Fatalf("a null at a learned hole bound as %v", binding)
	}

	// The rule behind it, over a spread of plans: whatever Bind accepts, Instantiate
	// takes back to the same plan.
	for name, p := range map[string][]PlanStep{
		"a value at the hole":          plan(float64(7)),
		"text at the hole":             plan("seven"),
		"an object at the hole":        plan(map[string]any{"k": []any{float64(1)}}),
		"false at the hole":            plan(false),
		"zero at the hole":             plan(float64(0)),
		"the empty string at the hole": plan(""),
		"a null at the hole":           plan(nil),
		"a different constant":         {{Action: "go", Args: map[string]any{"target": "base", "n": float64(7)}}},
	} {
		binding, ok := Bind(tmpl, p)
		if !ok {
			continue
		}
		got, err := Instantiate(tmpl, binding)
		if err != nil {
			t.Errorf("%s: Bind accepted the plan but Instantiate refused its binding: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, p) {
			t.Errorf("%s: Instantiate gave %+v, want the plan %+v", name, got, p)
		}
	}
}
