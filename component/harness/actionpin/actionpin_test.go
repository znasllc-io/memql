package actionpin

import "testing"

func TestParse_PinnedAndFloating(t *testing.T) {
	r, err := Parse("act_writeFile@3")
	if err != nil || r.ID != "act_writeFile" || r.Version != 3 || r.Floating {
		t.Fatalf("pinned parse wrong: %+v (%v)", r, err)
	}
	f, err := Parse("act_fmt")
	if err != nil || f.ID != "act_fmt" || !f.Floating {
		t.Fatalf("floating parse wrong: %+v (%v)", f, err)
	}
	// id with embedded namespace colons + an @version.
	n, err := Parse("v1:actions:action:abc@12")
	if err != nil || n.ID != "v1:actions:action:abc" || n.Version != 12 {
		t.Fatalf("namespaced pin parse wrong: %+v (%v)", n, err)
	}
}

func TestParse_Invalid(t *testing.T) {
	for _, bad := range []string{"", "act_x@0", "act_x@-1", "act_x@abc", "@3"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should error", bad)
		}
	}
}

func TestFormat_RoundTrip(t *testing.T) {
	if got := Format(Ref{ID: "act_x", Version: 3}); got != "act_x@3" {
		t.Fatalf("pinned format: want act_x@3, got %s", got)
	}
	if got := Format(Ref{ID: "act_x", Floating: true}); got != "act_x" {
		t.Fatalf("floating format: want act_x, got %s", got)
	}
}

func TestDecide(t *testing.T) {
	if Decide("fp", "fp") != Bump {
		t.Fatalf("matching fingerprints must bump")
	}
	if Decide("fp", "other") != Hold {
		t.Fatalf("mismatched fingerprints must hold")
	}
	if Decide("", "") != Hold {
		t.Fatalf("unverifiable (empty) must hold")
	}
}

func TestPlanUpgrade_BumpOnlyWhereVerified(t *testing.T) {
	bundles := []PinnedBundle{
		{BundleID: "b1", Ref: Ref{ID: "act_x", Version: 3}, RecordedFingerprint: "fp1"}, // will verify
		{BundleID: "b2", Ref: Ref{ID: "act_x", Version: 3}, RecordedFingerprint: "fp2"}, // will NOT verify
		{BundleID: "b3", Ref: Ref{ID: "act_x", Floating: true}},                          // floats -> bump, no re-run
	}
	// v4 reproduces fp1 for b1 but produces a different fp for b2.
	replay := func(b PinnedBundle) string {
		if b.BundleID == "b1" {
			return "fp1"
		}
		return "DIFFERENT"
	}
	results := PlanUpgrade(bundles, 4, replay)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	// b1 verified -> bumped to v4.
	if results[0].Decision != Bump || results[0].ToVersion != 4 || results[0].NewRef != "act_x@4" || results[0].Flagged {
		t.Fatalf("b1 should bump to act_x@4: %+v", results[0])
	}
	// b2 did not verify -> held on v3 + flagged.
	if results[1].Decision != Hold || results[1].ToVersion != 3 || results[1].NewRef != "act_x@3" || !results[1].Flagged {
		t.Fatalf("b2 should hold on act_x@3 flagged: %+v", results[1])
	}
	// b3 floats -> bumped, no re-run.
	if results[2].Decision != Bump || results[2].NewRef != "act_x" {
		t.Fatalf("b3 floating should bump: %+v", results[2])
	}
	if CountFlagged(results) != 1 {
		t.Fatalf("exactly one bundle should be flagged, got %d", CountFlagged(results))
	}
}

func TestPlanUpgrade_NilReplayHoldsPinned(t *testing.T) {
	// With no re-run function, a pinned bundle can't verify -> holds.
	bundles := []PinnedBundle{{BundleID: "b1", Ref: Ref{ID: "act_x", Version: 2}, RecordedFingerprint: "fp"}}
	results := PlanUpgrade(bundles, 3, nil)
	if results[0].Decision != Hold || !results[0].Flagged {
		t.Fatalf("nil replay must hold pinned bundle: %+v", results[0])
	}
}
