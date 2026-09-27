package work

import (
	"errors"
	"reflect"
	"testing"
)

// v builds a recorded version with an explicit basis.
func v(key string, version int, status string, basis Head) StepVersion {
	return StepVersion{Key: key, Version: version, Status: status, Basis: basis}
}

func h(pairs ...any) Head {
	out := Head{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = HeadEntry{Version: pairs[i+1].(int)}
	}
	return out
}

func TestPlanRerunExecutesFromTheStepAtMaxPlusOne(t *testing.T) {
	order := []string{"fetch", "draft", "publish"}
	versions := map[string][]StepVersion{
		"fetch":   {v("fetch", 1, "done", nil)},
		"draft":   {v("draft", 1, "done", nil), v("draft", 2, "done", nil)},
		"publish": {v("publish", 1, "done", nil)},
	}
	plan, err := PlanRerun(order, versions, "draft")
	if err != nil {
		t.Fatal(err)
	}
	if plan.From != "draft" {
		t.Errorf("From = %q, want draft", plan.From)
	}
	if want := map[string]int{"draft": 3, "publish": 2}; !reflect.DeepEqual(plan.Versions, want) {
		t.Errorf("Versions = %v, want %v: a re-run never reuses a version number, so it never reuses an idempotency key", plan.Versions, want)
	}
	if want := []string{"draft", "publish"}; !reflect.DeepEqual(plan.Stale, want) {
		t.Errorf("Stale = %v, want %v: the prefix is served, never run again", plan.Stale, want)
	}
}

func TestPlanRerunRefusesAnUnknownOrNestedKey(t *testing.T) {
	order := []string{"a", "b"}
	if _, err := PlanRerun(order, nil, "zz"); !errors.Is(err, ErrStepNotInRun) {
		t.Errorf("unknown key: err = %v, want ErrStepNotInRun", err)
	}
	if _, err := PlanRerun(order, nil, "a/inner"); !errors.Is(err, ErrNestedStep) {
		t.Errorf("nested key: err = %v, want ErrNestedStep", err)
	}
}

// Going back to the first answer of step a, after a re-run of a produced a
// whole second world, restores that first world without running anything.
func TestMovingTheHeadBackRestoresMatchingDownstreamVersions(t *testing.T) {
	order := []string{"a", "b", "c"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", h()), v("a", 2, "done", h())},
		"b": {v("b", 1, "done", h("a", 1)), v("b", 2, "done", h("a", 2))},
		"c": {v("c", 1, "done", h("a", 1, "b", 1)), v("c", 2, "done", h("a", 2, "b", 2))},
	}
	head, stale, err := MoveHead(order, h("a", 2, "b", 2, "c", 2), versions, "a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := h("a", 1, "b", 1, "c", 1); !head.Equal(want) {
		t.Errorf("head = %v, want %v", head, want)
	}
	if len(stale) != 0 {
		t.Errorf("stale = %v, want none: every downstream version computed from a1 still exists", stale)
	}
}

// The newest matching version wins: a person's own re-run of c against b1 is
// preferred over the pristine c1.
func TestMovingTheHeadRestoresTheNewestMatchingVersion(t *testing.T) {
	order := []string{"a", "b", "c"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", h())},
		"b": {v("b", 1, "done", h("a", 1)), v("b", 2, "done", h("a", 1))},
		"c": {
			v("c", 1, "done", h("a", 1, "b", 1)),
			v("c", 2, "done", h("a", 1, "b", 1)),
			v("c", 3, "done", h("a", 1, "b", 2)),
		},
	}
	head, stale, err := MoveHead(order, h("a", 1, "b", 2, "c", 3), versions, "b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := h("a", 1, "b", 1, "c", 2); !head.Equal(want) {
		t.Errorf("head = %v, want %v", head, want)
	}
	if len(stale) != 0 {
		t.Errorf("stale = %v, want none", stale)
	}
}

// "Moving the head re-runs only stale steps" (#5415 acceptance): the steps
// before the first unmatched one are restored or kept, and only that step and
// everything after it run again.
func TestMovingTheHeadReRunsOnlyFromTheFirstUnmatchedStep(t *testing.T) {
	order := []string{"a", "b", "c", "d"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", h()), v("a", 2, "done", h())},
		"b": {v("b", 1, "done", h("a", 1)), v("b", 2, "done", h("a", 2))},
		"c": {v("c", 2, "done", h("a", 2, "b", 2))},
		"d": {v("d", 1, "done", h("a", 1, "b", 1, "c", 1)), v("d", 2, "done", h("a", 2, "b", 2, "c", 2))},
	}
	head, stale, err := MoveHead(order, h("a", 2, "b", 2, "c", 2, "d", 2), versions, "a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c", "d"}; !reflect.DeepEqual(stale, want) {
		t.Errorf("stale = %v, want %v", stale, want)
	}
	if head["a"].Version != 1 || head["b"].Version != 1 {
		t.Errorf("head = %v: a and b must be current at version 1 and not run again", head)
	}
}

// A version that failed is never restored: the step after it is stale.
func TestAFailedVersionIsNeverRestored(t *testing.T) {
	order := []string{"a", "b", "c"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", h()), v("a", 2, "done", h())},
		"b": {v("b", 1, "done", h("a", 1)), v("b", 2, "done", h("a", 2))},
		"c": {v("c", 1, "failed", h("a", 1, "b", 1)), v("c", 2, "done", h("a", 2, "b", 2))},
	}
	_, stale, err := MoveHead(order, h("a", 2, "b", 2, "c", 2), versions, "a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c"}; !reflect.DeepEqual(stale, want) {
		t.Errorf("stale = %v, want %v: c1 failed, so c runs again", stale, want)
	}
	if _, _, err := MoveHead(order, h("a", 2, "b", 2, "c", 2), versions, "c", 1); !errors.Is(err, ErrVersionNotDone) {
		t.Errorf("making a failed version current: err = %v, want ErrVersionNotDone", err)
	}
}

// A version is made current with the upstream it was computed from.
func TestMakingAVersionCurrentBringsTheUpstreamItWasComputedFrom(t *testing.T) {
	order := []string{"a", "b", "c"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", h()), v("a", 2, "done", h())},
		"b": {v("b", 1, "done", h("a", 1)), v("b", 2, "done", h("a", 1)), v("b", 3, "done", h("a", 2))},
		"c": {v("c", 1, "done", h("a", 1, "b", 1)), v("c", 3, "done", h("a", 2, "b", 3))},
	}
	head, stale, err := MoveHead(order, h("a", 2, "b", 3, "c", 3), versions, "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if head["a"].Version != 1 {
		t.Errorf("a = %d, want 1: b2 was computed from a1, and keeping a2 would draw a run that never happened", head["a"].Version)
	}
	if want := []string{"c"}; !reflect.DeepEqual(stale, want) {
		t.Errorf("stale = %v, want %v: no c was computed from a1 and b2", stale, want)
	}
}

func TestMovingTheHeadRefusesAMissingVersion(t *testing.T) {
	order := []string{"a"}
	if _, _, err := MoveHead(order, h("a", 1), map[string][]StepVersion{"a": {v("a", 1, "done", nil)}}, "a", 7); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func TestMoveHeadNeverMutatesTheCallersHead(t *testing.T) {
	order := []string{"a", "b"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", h()), v("a", 2, "done", h())},
		"b": {v("b", 1, "done", h("a", 1))},
	}
	before := h("a", 2, "b", 1)
	if _, _, err := MoveHead(order, before, versions, "a", 1); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(h("a", 2, "b", 1)) {
		t.Errorf("the caller's head changed to %v", before)
	}
}

func TestForkAtPointsThePrefixAtTheSource(t *testing.T) {
	order := []string{"a", "b", "c"}
	source := Head{"a": {Version: 2}, "b": {Version: 1, RunId: "run-origin"}, "c": {Version: 1}}
	head, err := ForkAt(order, source, "run-src", "c")
	if err != nil {
		t.Fatal(err)
	}
	want := Head{"a": {Version: 2, RunId: "run-src"}, "b": {Version: 1, RunId: "run-origin"}}
	if !head.Equal(want) {
		t.Errorf("head = %v, want %v: the prefix is served by reference, and a reference keeps pointing at the run that holds the version", head, want)
	}
	if _, err := ForkAt(order, source, "run-src", "nope"); !errors.Is(err, ErrStepNotInRun) {
		t.Errorf("err = %v, want ErrStepNotInRun", err)
	}
}

func TestTheHeadRoundTripsThroughItsStoredForm(t *testing.T) {
	head := Head{"a": {Version: 2}, "b": {Version: 1, RunId: "run-src"}}
	// A stored head arrives off a row with numbers as float64.
	stored := map[string]any{
		"a": map[string]any{"version": float64(2)},
		"b": map[string]any{"version": float64(1), "runId": "run-src"},
	}
	if got := ParseHead(stored); !got.Equal(head) {
		t.Errorf("ParseHead = %v, want %v", got, head)
	}
	if got := ParseHead(head.Object()); !got.Equal(head) {
		t.Errorf("round trip = %v, want %v", got, head)
	}
	if ParseHead(nil) != nil || ParseHead(map[string]any{}) != nil {
		t.Error("an absent head must parse as nil, which readers treat as 'newest is current'")
	}
	if got := ParseHead(map[string]any{"a": "two", "b": map[string]any{"version": 1.5}, "c": map[string]any{"version": float64(0)}}); got != nil {
		t.Errorf("unreadable entries must be skipped, got %v", got)
	}
	if obj := (Head(nil)).Object(); obj == nil || len(obj) != 0 {
		t.Errorf("an empty head is written as {}, got %v", obj)
	}
}

func TestThePristineBasisIsOmittedAndReconstructed(t *testing.T) {
	order := []string{"a", "b", "c"}
	if !IsPristine(h("a", 1, "b", 1), order, "c") {
		t.Error("every earlier step at version 1 of this run is pristine")
	}
	if IsPristine(h("a", 1), order, "c") {
		t.Error("a basis missing an earlier step is not pristine: omitting it would invent an upstream")
	}
	if IsPristine(Head{"a": {Version: 1, RunId: "src"}, "b": {Version: 1}}, order, "c") {
		t.Error("a reference into another run is not pristine")
	}
	got := VersionOf(order, "c", 1, nil, "done")
	if !got.Basis.Equal(h("a", 1, "b", 1)) {
		t.Errorf("an absent stored basis reconstructs as pristine, got %v", got.Basis)
	}
	got = VersionOf(order, "c", 2, map[string]any{"a": map[string]any{"version": float64(2)}}, "done")
	if !got.Basis.Equal(h("a", 2)) {
		t.Errorf("a stored basis is read as stored, got %v", got.Basis)
	}
}

func TestBasisForTakesOnlyTheEarlierSteps(t *testing.T) {
	order := []string{"a", "b", "c"}
	if got := BasisFor(order, h("a", 2, "b", 3, "c", 4), "c"); !got.Equal(h("a", 2, "b", 3)) {
		t.Errorf("BasisFor = %v", got)
	}
	if got := BasisFor(order, h("a", 2, "b", 3), "a"); len(got) != 0 {
		t.Errorf("the first step has an empty basis, got %v", got)
	}
}

func TestNewestHeadIsWhatTheCollapsedReadsShow(t *testing.T) {
	order := []string{"a", "b"}
	versions := map[string][]StepVersion{
		"a": {v("a", 1, "done", nil), v("a", 3, "failed", nil)},
		"b": {v("b", 1, "done", nil)},
	}
	if got := NewestHead(order, versions); !got.Equal(h("a", 3, "b", 1)) {
		t.Errorf("NewestHead = %v", got)
	}
}
