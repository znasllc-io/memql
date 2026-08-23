package dslgate

import "testing"

// The COMPOSITE row-authz tier (memql#4312) renders
// `(<owner>==actor.userId)||(actor.isClusterOwner==true)`, and an author
// adjudicating a read over a composite-declared concept writes that same
// shape into the filter -- it is what the land gate asks for.
//
// That shape puts an admin gate under a top-level `||`, which is exactly
// the pattern adminCompositionViolation exists to refuse. Its reasoning
// is sound for the case it was written against and does NOT hold here,
// and the difference is the left operand:
//
//	fromE164==args.e164 || actor.isClusterOwner==true   // SELECTION -- fail-open
//	ownerUserId==actor.userId || actor.isClusterOwner==true   // OWNERSHIP -- the floor
//
// On the first, a false gate zeroes nothing: any caller supplying
// `fromE164` still reads rows. On the second, a false gate leaves exactly
// the caller's own rows, which is the tier's floor rather than a way
// around it.
const compositeFilterSource = `use worker.concepts.{ registration }

query registration myWorkersOrAll {
  filter  ownerUserId==actor.userId || actor.isClusterOwner==true
  shape   workerRegistrationFull
}
`

// The classification has to be OWNED. Reporting the composite's own
// spelling as ungated would push every future composite-declared read
// into the flagged bucket and train a reader to ignore the bucket.
func TestCompositeTierFilterClassifiesAsOwned(t *testing.T) {
	got := ClassifySource("worker/queries.memql", compositeFilterSource, Options{})
	if len(got) != 1 {
		t.Fatalf("classified %d constructs, want 1: %+v", len(got), got)
	}
	if got[0].Bucket != BucketOwned {
		t.Fatalf("the composite tier's own predicate classified as %q, want %q.\n"+
			"A false admin gate leaves the caller's OWN rows here, so ownership holds on every "+
			"path through the filter -- which is what the owned bucket means.", got[0].Bucket, BucketOwned)
	}
}

// And the admin-composition gate must not refuse it.
func TestCompositeTierFilterIsNotAnAdminCompositionViolation(t *testing.T) {
	for _, v := range ScanSource("worker/queries.memql", compositeFilterSource, Options{}) {
		if v.Gate == GateAdminGateComposition {
			t.Fatalf("the composite tier's own predicate was refused as a fail-open admin "+
				"composition: %s\n"+
				"The gate's premise is that a false admin gate zeroes nothing. Here the other arm "+
				"is the OWNERSHIP term, so a false gate leaves exactly the caller's own rows -- "+
				"the tier's floor, not a way around it. Refusing this shape makes the one form the "+
				"land gate asks an author to write unwritable (memql#4312).", v.Detail)
		}
	}
}

// NEGATIVE CONTROL for the change above. Widening the composition gate is
// exactly the direction that reopens memql#2839, so the fail-open shape it
// was written to refuse has to still be refused -- and by the same gate,
// not merely by a different test file that might get deleted with it.
func TestAdminGateOverASelectionTermIsStillRefused(t *testing.T) {
	const failOpen = `use telephony.concepts.{ call }

query call callsByNumber {
  filter  fromE164==args.e164 || actor.isClusterOwner==true
  shape   callFull
}
`
	found := false
	for _, v := range ScanSource("telephony/queries.memql", failOpen, Options{}) {
		if v.Gate == GateAdminGateComposition {
			found = true
		}
	}
	if !found {
		t.Fatal("`fromE164==args.e164 || actor.isClusterOwner==true` is no longer refused as a " +
			"fail-open admin composition. A false gate zeroes nothing here -- any caller " +
			"supplying fromE164 reads rows -- which is memql#2839 exactly. CallerScopeLeaf must " +
			"distinguish this from the composite tier by reading the OTHER arm: a selection term " +
			"satisfies neither half of it.")
	}
	// And it must not be misreported as owned either.
	got := ClassifySource("telephony/queries.memql", failOpen, Options{})
	if len(got) == 1 && got[0].Bucket == BucketOwned {
		t.Fatal("a selection term ORed with an admin gate classified as OWNED")
	}
}
