package memql

// authoring_breaker_pause_db_test.go -- the bundle-row half of a breaker's
// pause that a fake store cannot vouch for, against a REAL engine and a REAL
// Postgres (memql#6022): a paused bundle turned on again keeps no trace of the
// pause on its live row.
//
// Every bundle write read-merges: a field a write does not name is inherited
// from the row before it. A bundle id can be written again -- an email rule
// turned off and on restarts at version 1 under the same derived id -- so the
// activation itself must clear the reason a breaker paused it with, whatever
// the writes before it did or did not clear.
//
// Postgres-gated: skips when no DB is reachable, like the other _db_ tests in
// this package. The ids are unique per run, and nothing is truncated.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
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

	create := fmt.Sprintf(`mutation createAuthoringBundle(bundleId:%s, title:%s)`,
		languageParser.QuoteString(bundleId), languageParser.QuoteString("A paused bundle turned on again"))
	if _, err := eng.Execute(ctx, create); err != nil {
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
