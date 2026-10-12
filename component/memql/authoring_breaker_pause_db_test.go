package memql

// authoring_breaker_pause_db_test.go -- the bundle-row half of a breaker's
// pause that a fake store cannot vouch for, against a REAL engine and a REAL
// Postgres (memql#6022):
//
//   - a bundle turned on again keeps no trace of a pause or a retirement on its
//     live row. Every bundle write read-merges -- a field a write does not name
//     is inherited from the row before it -- and a bundle id can be written
//     again (an email rule turned off and on restarts at version 1 under the
//     same derived id), so the activation itself clears them;
//   - the pause is written against the version of the row it read: a retire
//     written elsewhere in between is never overwritten with paused.
//
// Postgres-gated: skips when no DB is reachable, like the other _db_ tests in
// this package. The ids are unique per run, and nothing is truncated.

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// TestAPausedBundleTurnedOnAgainCarriesNoFailureReason: the breaker's pause is
// written by the production write (PauseBundleForOwner) and the activation by
// the production write (SetBundleActive), with nothing in between. The live
// row reads active with no failure reason.
func TestAPausedBundleTurnedOnAgainCarriesNoFailureReason(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "u-pause-clear-" + run
	bundleId := "v1:authoring:bundle:pause-clear-" + run
	ctx := auth.ContextWithUserActor(context.Background(), owner)

	if err := serverCreatesBundle(eng, ctx, bundleId, "A paused bundle turned on again"); err != nil {
		t.Fatalf("create the bundle: %v", err)
	}
	activation := &engineActivationStore{engine: eng}
	if err := activation.SetBundleActive(ctx, bundleId); err != nil {
		t.Fatalf("activate the bundle: %v", err)
	}

	const reason = "Paused automatically after 5 failed runs in a row."
	if err := (&engineArmStore{engine: eng}).PauseBundleForOwner(context.Background(), owner, bundleId, reason); err != nil {
		t.Fatalf("record the breaker's pause: %v", err)
	}
	if p := latestPayload(t, ctx, db, "v1:authoring:bundle", bundleId); p["status"] != "paused" || p["failureReason"] != reason {
		t.Fatalf("the pause wrote status %v, failureReason %v; want paused, %q", p["status"], p["failureReason"], reason)
	}

	if err := activation.SetBundleActive(ctx, bundleId); err != nil {
		t.Fatalf("turn the bundle on again: %v", err)
	}
	p := latestPayload(t, ctx, db, "v1:authoring:bundle", bundleId)
	if p["status"] != "active" {
		t.Fatalf("the bundle turned on again reads %v, want active", p["status"])
	}
	if got, _ := p["failureReason"].(string); got != "" {
		t.Fatalf("the bundle turned on again reads active with failureReason %q; a live bundle carries no failure reason", got)
	}
}

// TestARetiredBundleTurnedOnAgainCarriesNoRetirement: an email rule turned off
// and on again is the same bundle id retired and then activated. The live row
// carries no retiredAt -- the concept says it is null unless the bundle is
// retired.
func TestARetiredBundleTurnedOnAgainCarriesNoRetirement(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "u-retire-clear-" + run
	bundleId := "v1:authoring:bundle:retire-clear-" + run
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	if err := serverCreatesBundle(eng, ctx, bundleId, "A bundle turned off and on"); err != nil {
		t.Fatalf("create the bundle: %v", err)
	}
	activation := &engineActivationStore{engine: eng}
	if err := activation.SetBundleActive(ctx, bundleId); err != nil {
		t.Fatalf("activate the bundle: %v", err)
	}
	if err := activation.SetBundleRetired(ctx, bundleId); err != nil {
		t.Fatalf("retire the bundle: %v", err)
	}
	if p := latestPayload(t, ctx, db, "v1:authoring:bundle", bundleId); p["status"] != "retired" || p["retiredAt"] == nil || p["retiredAt"] == "" {
		t.Fatalf("the retire wrote status %v, retiredAt %v", p["status"], p["retiredAt"])
	}

	if err := activation.SetBundleActive(ctx, bundleId); err != nil {
		t.Fatalf("turn the bundle on again: %v", err)
	}
	p := latestPayload(t, ctx, db, "v1:authoring:bundle", bundleId)
	if p["status"] != "active" {
		t.Fatalf("the bundle turned on again reads %v, want active", p["status"])
	}
	if got, _ := p["retiredAt"].(string); got != "" {
		t.Fatalf("the bundle turned on again reads active with retiredAt %q; only a retired bundle carries one", got)
	}
}

// retireDuringPause is the production store with another node's retire landing
// in the instant between the pause's fresh read of the bundle and its write.
type retireDuringPause struct {
	*engineArmStore
	retire func() error
}

func (s *retireDuringPause) PauseBundleForOwner(ctx context.Context, owner, bundleId, reason string) error {
	if retire := s.retire; retire != nil {
		s.retire = nil
		if err := retire(); err != nil {
			return fmt.Errorf("the other node's retire: %w", err)
		}
	}
	return s.engineArmStore.PauseBundleForOwner(ctx, owner, bundleId, reason)
}

// TestAPauseNeverOverwritesARetireWrittenElsewhere: a breaker's pause reads the
// bundle fresh and writes against the version it read. Another node retires the
// bundle in between -- the person turned the rule off -- so the write is refused
// as written against a row that changed, the bundle keeps reading retired
// (paused would read as resumable), nothing is reported as paused, and the
// bundle still comes out of this node.
func TestAPauseNeverOverwritesARetireWrittenElsewhere(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "u-fenced-pause-" + run
	bundleId := "v1:authoring:bundle:fenced-pause-" + run
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	if err := serverCreatesBundle(eng, ctx, bundleId, "A bundle retired while it tripped"); err != nil {
		t.Fatalf("create the bundle: %v", err)
	}
	if err := serverWritesConstruct(eng, ctx, "v1:authoring:construct:fenced-pause-"+run, bundleId, "autoFencedPause"); err != nil {
		t.Fatalf("setup: the bundle's construct: %v", err)
	}
	activation := &engineActivationStore{engine: eng}
	if err := activation.SetBundleActive(ctx, bundleId); err != nil {
		t.Fatalf("activate the bundle: %v", err)
	}
	// The owner as the row stores it -- the spelling a node registers under
	// and a trip names.
	stored, _ := latestPayload(t, ctx, db, "v1:authoring:bundle", bundleId)["ownerUserId"].(string)
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	if err := registerBundleConstructs([]*AuthoredConstruct{{OwnerUserId: stored, Kind: "automation", Name: "autoFencedPause",
		BundleId: bundleId, Version: 1, Source: "automation autoFencedPause { }"}}, deps); err != nil {
		t.Fatalf("setup: hold the bundle: %v", err)
	}
	var told atomic.Int32
	listener := "fenced-pause-" + run
	eng.SetAuthoredPauseListener(listener, func(context.Context, string, string) error { told.Add(1); return nil })
	t.Cleanup(func() { eng.SetAuthoredPauseListener(listener, nil) })

	retired := false
	store := &retireDuringPause{engineArmStore: &engineArmStore{engine: eng}, retire: func() error {
		retired = true
		return activation.SetBundleRetired(ctx, bundleId)
	}}
	outcome, err := eng.pauseTrippedAutomationWithStore(context.Background(), store, deps, AuthoredTrip{
		Owner: stored, Automation: "autoFencedPause", BundleId: bundleId, Version: 1,
		Reason: "Paused automatically after 5 failed runs in a row.",
	})
	if !retired {
		t.Fatal("setup: the other node's retire never landed between the read and the write")
	}
	if err != nil || outcome != TripBundleNotActive {
		t.Fatalf("a trip whose bundle was retired in between did %q, %v; want %q", outcome, err, TripBundleNotActive)
	}
	if st := bundleStatusRow(t, db, bundleId); st != "retired" {
		t.Fatalf("the bundle reads %q: a pause overwrote the retire written between its read and its write", st)
	}
	if n := told.Load(); n != 0 {
		t.Fatalf("a pause that was not written was reported %d times", n)
	}
	if _, held := deps.Registry.Lookup(stored, "automation", "autoFencedPause"); held {
		t.Fatal("a retired bundle stayed on the node whose breaker tripped")
	}
}
