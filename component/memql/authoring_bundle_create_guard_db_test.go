package memql

// authoring_bundle_create_guard_db_test.go -- a bundle comes to exist only as a
// server pipeline's write (memql#6022, Task 3b fix round 4), against a REAL
// engine and a REAL Postgres: the @serverOnly gate on createAuthoringBundle,
// the raw-write short-circuit and the bundle create guard.
//
// Postgres-gated: skips when no DB is reachable, like the other _db_ tests in
// this package. The ids are unique per run, and nothing is truncated.

import (
	"fmt"
	"os"
	"testing"
	"time"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestABundleComesToExistOnlyAsAServerWrite: a client -- even one creating a
// bundle that would be their own -- cannot bring a bundle into existence,
// through the mutation or a raw insert, draft or born active; nothing lands at
// the id. The engine's bundle writer can, and stamps the bundle its caller's.
// The guard is about creation: the owner's own write to the bundle that now
// exists still lands, and re-running the mutation over it is still the
// server's alone.
func TestABundleComesToExistOnlyAsAServerWrite(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "u-create-guard-" + run
	client := rowAuthzCallerCtx(owner)
	bundleId := bundleConceptID + ":create-guard-" + run

	if _, err := eng.Execute(client, fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "Mine")`, languageParser.QuoteString(bundleId))); err == nil {
		t.Error("a client created a bundle through createAuthoringBundle")
	}
	_, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload={"title":"Mine","status":"draft"})`, bundleConceptID, bundleId))
	refusedBecause(t, err, "creating a bundle refused", "a client's raw insert of a new bundle")
	_, err = eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload={"title":"Mine","status":"active"})`, bundleConceptID, bundleId))
	refusedBecause(t, err, "creating a bundle refused", "a client's raw insert of a new bundle born active")
	if row := latestRow(t, db, bundleConceptID, bundleId); row != nil {
		t.Fatalf("a client's refused writes left a bundle at %s: %v", bundleId, row)
	}

	if err := serverCreatesBundle(eng, client, bundleId, "The server's"); err != nil {
		t.Fatalf("the engine's bundle writer: %v", err)
	}
	row := latestRow(t, db, bundleConceptID, bundleId)
	if row == nil || row["status"] != "draft" || BareShortId(fmt.Sprint(row["ownerUserId"])) != owner {
		t.Fatalf("the engine's bundle writer left %v, want a draft owned by %s", row, owner)
	}

	if _, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload={"title":"Renamed by its owner"})`, bundleConceptID, bundleId)); err != nil {
		t.Fatalf("the owner's write to their own existing bundle: %v", err)
	}
	if got := latestRow(t, db, bundleConceptID, bundleId)["title"]; got != "Renamed by its owner" {
		t.Fatalf("the owner's write to their own bundle left the title %v", got)
	}
	if _, err := eng.Execute(client, fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "Again")`, languageParser.QuoteString(bundleId))); err == nil {
		t.Error("a client ran createAuthoringBundle over their own existing bundle")
	}
}
