package memql

// authoring_bundle_verdict_guard_db_test.go -- a bundle's gate verdicts are the
// server's claims (memql#6022, Task 3b fix round 5), against a REAL engine and a
// REAL Postgres: the @serverOnly gate on the two recorders, the raw-write
// short-circuit and the verdict guard.
//
// Postgres-gated: skips when no DB is reachable, like the other _db_ tests in
// this package. The ids are unique per run, and nothing is truncated.

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestAGateVerdictIsAServerClaim: an owner cannot record either gate's verdict
// on their own bundle that no gate ran on -- through the recorders, or through
// a raw write of the passing status or of a report -- and nothing moves. The
// server records both. The guard is about the claim: the owner's own write
// that leaves the verdicts as they are still lands.
func TestAGateVerdictIsAServerClaim(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "u-verdict-" + run
	client := rowAuthzCallerCtx(owner)
	bundleId := bundleConceptID + ":verdict-" + run
	q := languageParser.QuoteString
	if err := serverCreatesBundle(eng, client, bundleId, "Never gated"); err != nil {
		t.Fatalf("setup: the bundle: %v", err)
	}

	for _, c := range []struct{ name, call string }{
		{"Gate 1's verdict through recordBundleValidation", fmt.Sprintf(`mutation recordBundleValidation(bundleId: %s, status: "validated", validationReport: {ok: true})`, q(bundleId))},
		{"Gate 2's verdict through recordBundleDryRun", fmt.Sprintf(`mutation recordBundleDryRun(bundleId: %s, status: "dryRunPassed", dryRunReport: {ok: true})`, q(bundleId))},
	} {
		if _, err := eng.Execute(client, c.call); err == nil {
			t.Errorf("the owner recorded %s on a bundle no gate ran on", c.name)
		}
	}
	for _, c := range []struct{ name, payload string }{
		{"validated", `{"status":"validated"}`},
		{"dryRunPassed", `{"status":"dryRunPassed"}`},
		{"a validation report", `{"validationReport":{"ok":true}}`},
		{"a dry-run report", `{"dryRunReport":{"ok":true}}`},
	} {
		_, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload=%s)`, bundleConceptID, bundleId, c.payload))
		refusedBecause(t, err, "gate verdict", "the owner's raw write of "+c.name)
	}
	row := latestRow(t, db, bundleConceptID, bundleId)
	if row["status"] != "draft" || row["validationReport"] != nil || row["dryRunReport"] != nil {
		t.Fatalf("the owner's refused writes moved the bundle: status %v, validationReport %v, dryRunReport %v",
			row["status"], row["validationReport"], row["dryRunReport"])
	}

	// The server records both verdicts.
	mustExecute(t, eng, auth.ContextWithInternalOrigin(client), fmt.Sprintf(`mutation recordBundleValidation(bundleId: %s, status: "validated", validationReport: {ok: true})`, q(bundleId)))
	mustExecute(t, eng, auth.ContextWithInternalOrigin(client), fmt.Sprintf(`mutation recordBundleDryRun(bundleId: %s, status: "dryRunPassed", dryRunReport: {ok: true})`, q(bundleId)))
	if st := latestRow(t, db, bundleConceptID, bundleId)["status"]; st != "dryRunPassed" {
		t.Fatalf("the server's verdicts left the bundle %v, want dryRunPassed", st)
	}

	// What leaves the verdicts as they are is still the owner's.
	if _, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload={"title":"Renamed by its owner"})`, bundleConceptID, bundleId)); err != nil {
		t.Fatalf("the owner's write that leaves the verdicts alone: %v", err)
	}
	row = latestRow(t, db, bundleConceptID, bundleId)
	if row["title"] != "Renamed by its owner" || row["status"] != "dryRunPassed" {
		t.Fatalf("after the owner's rename the bundle reads title %v, status %v", row["title"], row["status"])
	}
}
