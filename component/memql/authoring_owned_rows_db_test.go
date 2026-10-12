package memql

// authoring_owned_rows_db_test.go -- authored bundles and constructs are owned
// rows, and a gated bundle arms only what its gates saw (memql#6022, Task 3b
// fix round 3), against a REAL engine and a REAL Postgres: the parser, the
// @serverOnly gate, row authz on reads and writes, the raw-write
// short-circuit, read-merge, and the two write guards.
//
// Postgres-gated: skips when no DB is reachable, like the other _db_ tests in
// this package. The ids are unique per run, and nothing is truncated.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

const constructConcept = "v1:authoring:construct"

// latestRow reads the newest version of a row straight off the table, or nil
// when there is none.
func latestRow(t *testing.T, db *bun.DB, concept, id string) map[string]any {
	t.Helper()
	var raw []byte
	err := db.QueryRowContext(context.Background(),
		`SELECT payload FROM "MemoryNodes" WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC LIMIT 1`, concept, id).Scan(&raw)
	if err != nil {
		return nil
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode %s %s: %v", concept, id, err)
	}
	return p
}

// refusedBecause fails the test unless err is a refusal that names why --
// several checks refuse the same write, and a refusal for another reason
// would hide the one a test is about.
func refusedBecause(t *testing.T, err error, why, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s was accepted", what)
		return
	}
	if !strings.Contains(err.Error(), why) {
		t.Errorf("%s was refused, but not because %q: %v", what, why, err)
	}
}

// serverWritesConstruct is a gate-running pipeline's write of a member
// construct: createAuthoringConstruct under internal origin, as the owner.
func serverWritesConstruct(eng *MemQLEngine, owner context.Context, constructId, bundleId, name string) error {
	_, err := eng.Execute(auth.ContextWithInternalOrigin(owner), fmt.Sprintf(
		`mutation createAuthoringConstruct(constructId: %s, bundleId: %s, kind: "automation", name: %s, targetNamespace: "campaigns", source: %s)`,
		languageParser.QuoteString(constructId), languageParser.QuoteString(bundleId),
		languageParser.QuoteString(name), languageParser.QuoteString("automation "+name+" { }")))
	return err
}

// TestASecondUserCannotTouchAnotherUsersBundle: the bundle and its construct
// are the first user's. A second user -- an ordinary writer -- can neither read
// them nor retire, pause, rewrite or take either over; each attempt is refused
// and the rows read as they did. The owner still can.
func TestASecondUserCannotTouchAnotherUsersBundle(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	a, b := rowAuthzCallerCtx("u-first-"+run), rowAuthzCallerCtx("u-second-"+run)
	bundleId := bundleConceptID + ":owned-" + run
	constructId := constructConcept + ":owned-" + run
	mustExecute(t, eng, a, fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "The first user's rule")`, languageParser.QuoteString(bundleId)))
	if err := serverWritesConstruct(eng, a, constructId, bundleId, "ownedRule"); err != nil {
		t.Fatalf("the first user's construct: %v", err)
	}
	if err := (&engineActivationStore{engine: eng}).SetBundleActive(a, bundleId); err != nil {
		t.Fatalf("the first user's gated activation: %v", err)
	}
	bundleBefore := latestRow(t, db, bundleConceptID, bundleId)
	constructBefore := latestRow(t, db, constructConcept, constructId)
	if bundleBefore["status"] != "active" || constructBefore == nil {
		t.Fatalf("setup: bundle %v, construct %v", bundleBefore["status"], constructBefore)
	}

	for _, read := range []string{
		fmt.Sprintf(`query authoringBundleById(bundleId: %s)`, languageParser.QuoteString(bundleId)),
		fmt.Sprintf(`query authoringConstructsForBundle(bundleId: %s)`, languageParser.QuoteString(bundleId)),
	} {
		res, err := eng.Execute(b, read)
		if err == nil && len(MaterializeRows(res)) != 0 {
			t.Errorf("the second user read the first user's rows: %s returned %d", read, len(MaterializeRows(res)))
		}
	}

	for _, c := range []struct{ name, call string }{
		{"retire the bundle", fmt.Sprintf(`mutation retireAuthoringBundle(bundleId: %s)`, languageParser.QuoteString(bundleId))},
		{"pause the bundle", fmt.Sprintf(`mutation setAuthoringBundleStatus(bundleId: %s, status: "paused")`, languageParser.QuoteString(bundleId))},
		{"pause the bundle with a raw write", fmt.Sprintf(`insert("%s", id="%s", payload={"status":"paused"})`, bundleConceptID, bundleId)},
		{"rewrite the bundle", fmt.Sprintf(`insert("%s", id="%s", payload={"title":"Hijacked"})`, bundleConceptID, bundleId)},
		{"take the bundle over through a gate recorder", fmt.Sprintf(`mutation recordBundleValidation(bundleId: %s, status: "validated", validationReport: {ok: true})`, languageParser.QuoteString(bundleId))},
		{"take the bundle over by creating it again", fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "Mine now")`, languageParser.QuoteString(bundleId))},
		{"retire the construct", fmt.Sprintf(`mutation setConstructStatus(constructId: %s, status: "retired")`, languageParser.QuoteString(constructId))},
		{"rewrite the construct", fmt.Sprintf(`insert("%s", id="%s", payload={"name":"hijacked"})`, constructConcept, constructId)},
		{"take the construct over by cataloguing it", fmt.Sprintf(`mutation catalogueConstruct(constructId: %s, catalogKey: "k", catalogMatchText: "t", fromBundleId: %s)`, languageParser.QuoteString(constructId), languageParser.QuoteString(bundleId))},
	} {
		if _, err := eng.Execute(b, c.call); err == nil {
			t.Errorf("the second user could %s", c.name)
		}
	}
	bundleAfter := latestRow(t, db, bundleConceptID, bundleId)
	constructAfter := latestRow(t, db, constructConcept, constructId)
	for _, k := range []string{"status", "ownerUserId", "title"} {
		if bundleAfter[k] != bundleBefore[k] {
			t.Errorf("the bundle's %s moved from %v to %v under the second user's writes", k, bundleBefore[k], bundleAfter[k])
		}
	}
	for _, k := range []string{"status", "ownerUserId", "name", "catalogued"} {
		if constructAfter[k] != constructBefore[k] {
			t.Errorf("the construct's %s moved from %v to %v under the second user's writes", k, constructBefore[k], constructAfter[k])
		}
	}

	// The refusals are about who wrote, not about the writes: the owner turns
	// their own bundle off.
	if _, err := eng.Execute(a, fmt.Sprintf(`mutation retireAuthoringBundle(bundleId: %s)`, languageParser.QuoteString(bundleId))); err != nil {
		t.Fatalf("the owner retiring their own bundle: %v", err)
	}
	if st := latestRow(t, db, bundleConceptID, bundleId)["status"]; st != "retired" {
		t.Fatalf("the owner's retire left the bundle reading %v", st)
	}
}

// TestAGatedBundleArmsOnlyWhatItsGateSaw: a construct joins a bundle only as
// the gate-running pipeline's write, while the bundle's gates have not run.
// One created in advance under a bundle id that does not exist yet is refused,
// and so is one added to the bundle once it is active -- from a client or from
// the server -- so the activation and every reconcile pass after it arm exactly
// the construct the dry run saw.
func TestAGatedBundleArmsOnlyWhatItsGateSaw(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := rowAuthzCallerCtx("u-gated-" + run)
	bundleId := bundleConceptID + ":gated-" + run

	// In advance, under the id the bundle will have.
	if _, err := eng.Execute(owner, fmt.Sprintf(`mutation createAuthoringConstruct(constructId: %s, bundleId: %s, kind: "automation", name: "squatter", targetNamespace: "campaigns", source: "automation squatter { }")`,
		languageParser.QuoteString(constructConcept+":squatter-client-"+run), languageParser.QuoteString(bundleId))); err == nil {
		t.Error("a client created a construct under a bundle id that does not exist yet")
	}
	refusedBecause(t, serverWritesConstruct(eng, owner, constructConcept+":squatter-server-"+run, bundleId, "squatterServer"),
		"does not exist", "a construct created under a bundle id that does not exist yet")

	// The gated pipeline: the bundle, its one construct, both gate verdicts,
	// the activation.
	mustExecute(t, eng, owner, fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: "Gated")`, languageParser.QuoteString(bundleId)))
	// Not even into the owner's own draft from a client, and not from the
	// pipeline as somebody else.
	_, err := eng.Execute(owner, fmt.Sprintf(`insert("%s", id="%s", payload={"bundleId":%q,"kind":"automation","name":"clientDraft","targetNamespace":"campaigns","source":"automation clientDraft { }","status":"draft"})`,
		constructConcept, constructConcept+":client-draft-"+run, bundleId))
	refusedBecause(t, err, "internal origin", "a client's raw insert of a construct into its own draft bundle")
	refusedBecause(t, serverWritesConstruct(eng, rowAuthzCallerCtx("u-other-"+run), constructConcept+":other-"+run, bundleId, "otherOwner"),
		"belongs to someone else", "a construct owned by someone else joining the bundle")
	if err := serverWritesConstruct(eng, owner, constructConcept+":gated-"+run, bundleId, "gatedOne"); err != nil {
		t.Fatalf("the pipeline's construct, while the bundle is a draft: %v", err)
	}
	mustExecute(t, eng, auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`mutation recordBundleValidation(bundleId: %s, status: "validated", validationReport: {ok: true})`, languageParser.QuoteString(bundleId)))
	mustExecute(t, eng, auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`mutation recordBundleDryRun(bundleId: %s, status: "dryRunPassed", dryRunReport: {ok: true})`, languageParser.QuoteString(bundleId)))
	refusedBecause(t, serverWritesConstruct(eng, owner, constructConcept+":after-dryrun-"+run, bundleId, "afterDryRun"),
		"reads dryRunPassed", "a construct joining a bundle that had passed its dry run")
	// The activation runs as the owner the row names, as the gated callers'
	// own envelope does.
	stored, _ := latestRow(t, db, bundleConceptID, bundleId)["ownerUserId"].(string)
	deps := (&armHooks{}).deps(NewAuthoredRuntimeRegistry())
	if _, err := ActivateApprovedBundleWithStore(rowAuthzCallerCtx(stored), &engineActivationStore{engine: eng}, stored, bundleId, deps); err != nil {
		t.Fatalf("the gated activation: %v", err)
	}

	// After it is live, from a client and from the server.
	_, err = eng.Execute(owner, fmt.Sprintf(`insert("%s", id="%s", payload={"bundleId":%q,"kind":"automation","name":"injectedLater","targetNamespace":"campaigns","source":"automation injectedLater { }","status":"draft"})`,
		constructConcept, constructConcept+":injected-client-"+run, bundleId))
	refusedBecause(t, err, "internal origin", "a client's raw insert of a construct into an active bundle")
	refusedBecause(t, serverWritesConstruct(eng, owner, constructConcept+":injected-server-"+run, bundleId, "injectedServer"),
		"reads active", "a construct added to an active bundle")
	_, err = eng.Execute(auth.ContextWithInternalOrigin(owner), fmt.Sprintf(`insert("%s", id="%s", payload={"source":"automation gatedOne { step }"})`,
		constructConcept, constructConcept+":gated-"+run))
	refusedBecause(t, err, "reads active", "a rewrite of the source of a construct in an active bundle")
	// What is not membership stays the owner's: the OS retires a construct by
	// its status.
	if _, err := eng.Execute(owner, fmt.Sprintf(`mutation setConstructStatus(constructId: %s, status: "active")`,
		languageParser.QuoteString(constructConcept+":gated-"+run))); err != nil {
		t.Fatalf("the owner's status write on their own construct: %v", err)
	}

	// Every replica's reconcile arms exactly the construct the dry run saw.
	recon := (&armHooks{}).deps(NewAuthoredRuntimeRegistry())
	if _, err := reconcileWithStore(context.Background(), &engineArmStore{engine: eng}, recon); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var armed []string
	for _, h := range recon.Registry.ListAll() {
		if BareShortId(h.BundleId) == BareShortId(bundleId) {
			armed = append(armed, h.Kind+"/"+h.Name)
		}
	}
	if strings.Join(armed, ",") != "automation/gatedOne" {
		t.Fatalf("the reconcile pass armed %v from the gated bundle; want exactly the construct its dry run saw", armed)
	}
}
