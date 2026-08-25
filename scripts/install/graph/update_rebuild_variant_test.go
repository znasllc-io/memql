package graph

import (
	"reflect"
	"testing"
)

// update_rebuild_variant_test.go -- znasllc-io/memql#4578.
//
// THE ASSERTION: "Update from origin and rebuild" builds EXACTLY the way
// "Rebuild from checkout" does. The two are one crossing with and without a
// fetch in front, they are offered side by side on the same row, and an
// operator choosing between them is choosing whether to fetch -- not between
// two builds.
//
// So the second step of update-rebuild.json and the only step of rebuild.json
// must be the same step. Every field: the same script, the same pinned
// `--image-source=checkout`, the same timeout, the same receipt, the same
// verify predicate, the same operator sentence.
//
// EXCEPT ONE, and the exception is the point of the second document. The
// dependency edge is what makes the update run FIRST -- TopoOrder returns
// WAVES, so without it the build would run concurrently with the update,
// reading a working tree while git rewrites it and producing an image whose
// source nothing on the machine can name.
//
// Why a gate rather than a sentence in a comment: the failure it prevents is
// silent in the worst way. Change the rebuild step's params, its verify or its
// timeout in one document and not the other, and both buttons still work --
// they simply stop doing the same thing, and the difference shows up as a
// cluster that came out different depending on which one you pressed.
func TestTheUpdateRebuildStepIsTheRebuildStep(t *testing.T) {
	rebuild := mustLoadEmbedded(t, Rebuild)
	update := mustLoadEmbedded(t, UpdateRebuild)

	if len(rebuild.Steps) != 1 {
		t.Fatalf("rebuild.json has %d steps; this gate assumes the one it has always had", len(rebuild.Steps))
	}
	from := rebuild.Steps[0]

	var to *Step
	for i := range update.Steps {
		if update.Steps[i].ID == from.ID {
			to = &update.Steps[i]
		}
	}
	if to == nil {
		t.Fatalf("update-rebuild.json has no %q step, so it is not the rebuild with a fetch in front -- "+
			"it is a second way to build, which is the thing this pair exists not to be", from.ID)
	}

	// The edge is the ONE deliberate difference, and it is asserted rather than
	// merely excused: an update-rebuild whose build does not wait is the exact
	// race this document exists to prevent.
	if got, want := to.DependsOn, []string{"updateCheckout"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s depends on %v, want %v -- TopoOrder returns waves, so without the edge the "+
			"build runs CONCURRENTLY with the update and reads a tree git is rewriting",
			to.ID, got, want)
	}
	if len(from.DependsOn) != 0 {
		t.Errorf("rebuild.json's %s has dependencies %v; it is a one-step graph", from.ID, from.DependsOn)
	}

	// Everything else must match. Compared with the edge zeroed rather than
	// field by field, so a field added to Step in future is covered on the day
	// it is added rather than on the day somebody remembers this file.
	a, b := from, *to
	a.DependsOn = nil
	b.DependsOn = nil
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the two rebuild steps differ by more than their dependency edge.\n"+
			"  rebuild.json:        %+v\n"+
			"  update-rebuild.json: %+v\n"+
			"Both buttons would still work; they would simply stop building the same way, "+
			"which shows up as a cluster that came out different depending on which was pressed.", a, b)
	}
}
