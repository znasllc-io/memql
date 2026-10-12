package memql

// authoring_bundle_activation_guard_db_test.go -- a client cannot get a bundle
// armed that the gates never passed (memql#6022, Task 3b fix round 2).
//
// Every replica arms every bundle that reads active: the boot re-arm, each
// reconcile pass and the arm broadcast all follow the bundle row. So `active`
// has to mean the gated activation ran. These drive the whole write path
// against a real Postgres -- parser, the @serverOnly gate, the raw-write
// short-circuit, row authz, read-merge and the write guard -- as the bundle's
// OWNER, from a client: the owner is exactly who the gates are for, and row
// authz admits them to their own rows.
//
// Postgres-gated: skips when no DB is reachable, like the other _db_ tests in
// this package. The ids are unique per run, and nothing is truncated.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
)

const bundleConceptID = "v1:authoring:bundle"

// TestAClientCannotGetAnUngatedBundleArmed: every way a client can write the
// bundle row -- the activation mutation, the generic status mutation, the raw
// insert literal onto the row or creating one, and the update form a person's
// own authored mutation reaches -- leaves it not active, so no replica arms it.
// The gated server path still activates, and the same reconcile pass arms what
// it activated, so the negative is not a reconcile that arms nothing at all.
func TestAClientCannotGetAnUngatedBundleArmed(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	run := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "u-ungated-" + run
	// A client's call: the bundle's own owner, an ordinary writer, and no
	// internal origin.
	client := rowAuthzCallerCtx(owner)

	// draft writes one bundle with one automation, as a client may.
	draft := func(t *testing.T, name string) (bundleId, automation string) {
		t.Helper()
		bundleId = bundleConceptID + ":ungated-" + name + "-" + run
		automation = "ungated" + camel(name)
		mustExecute(t, eng, client, fmt.Sprintf(`mutation createAuthoringBundle(bundleId: %s, title: %s)`,
			languageParser.QuoteString(bundleId), languageParser.QuoteString("An ungated bundle")))
		mustExecute(t, eng, client, fmt.Sprintf(`mutation createAuthoringConstruct(constructId: %s, bundleId: %s, kind: "automation", name: %s, targetNamespace: "campaigns", source: %s)`,
			languageParser.QuoteString("v1:authoring:construct:ungated-"+name+"-"+run), languageParser.QuoteString(bundleId),
			languageParser.QuoteString(automation), languageParser.QuoteString("automation "+automation+" { }")))
		return bundleId, automation
	}

	var ungated []string
	for _, c := range []struct {
		name  string
		write func(bundleId string) error
	}{
		{"activation-mutation", func(id string) error {
			_, err := eng.Execute(client, fmt.Sprintf(`mutation activateAuthoringBundle(bundleId: %s)`, languageParser.QuoteString(id)))
			return err
		}},
		{"status-mutation", func(id string) error {
			_, err := eng.Execute(client, fmt.Sprintf(`mutation setAuthoringBundleStatus(bundleId: %s, status: "active")`, languageParser.QuoteString(id)))
			return err
		}},
		{"raw-insert", func(id string) error {
			_, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload={"status":"active"})`, bundleConceptID, id))
			return err
		}},
		{"raw-update", func(id string) error {
			return rawBundleUpdate(t, client, eng, id, map[string]any{"status": "active"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			bundleId, _ := draft(t, c.name)
			ungated = append(ungated, bundleId)
			if err := c.write(bundleId); err == nil {
				t.Errorf("a client's %s was accepted", c.name)
			}
			if st := bundleStatusRow(t, db, bundleId); st == string(BundleActive) {
				t.Errorf("a client's %s left the bundle reading active", c.name)
			}
		})
	}
	t.Run("raw-insert-of-a-new-active-bundle", func(t *testing.T) {
		bundleId := bundleConceptID + ":ungated-new-" + run
		ungated = append(ungated, bundleId)
		mustExecute(t, eng, client, fmt.Sprintf(`mutation createAuthoringConstruct(constructId: %s, bundleId: %s, kind: "automation", name: "ungatedNew", targetNamespace: "campaigns", source: "automation ungatedNew { }")`,
			languageParser.QuoteString("v1:authoring:construct:ungated-new-"+run), languageParser.QuoteString(bundleId)))
		payload, _ := json.Marshal(map[string]any{"title": "Born active", "status": "active", "version": 1})
		if _, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload=%s)`, bundleConceptID, bundleId, payload)); err == nil {
			t.Errorf("a client's raw insert of a new active bundle was accepted")
		}
		if st := bundleStatusRow(t, db, bundleId); st == string(BundleActive) {
			t.Errorf("a client's raw insert created a bundle reading active")
		}
	})

	// The gated server path, for contrast.
	gated, gatedAutomation := draft(t, "gated")
	if err := (&engineActivationStore{engine: eng}).SetBundleActive(client, gated); err != nil {
		t.Fatalf("the gated activation's own write: %v", err)
	}

	// The guard is about BECOMING active: the owner's own write that leaves an
	// active bundle active -- renaming it -- is not an activation.
	if _, err := eng.Execute(client, fmt.Sprintf(`insert("%s", id="%s", payload={"title":"Renamed while live"})`, bundleConceptID, gated)); err != nil {
		t.Fatalf("the owner's rename of their active bundle was refused: %v", err)
	}
	if st := bundleStatusRow(t, db, gated); st != string(BundleActive) {
		t.Fatalf("the owner's rename left their bundle reading %q", st)
	}

	// No replica arms what no gate passed: a reconcile pass over the database
	// -- what every replica runs -- arms the gated bundle and nothing else here.
	hooks := &armHooks{}
	deps := hooks.deps(NewAuthoredRuntimeRegistry())
	if _, err := reconcileWithStore(context.Background(), &engineArmStore{engine: eng}, deps); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	armed := map[string]bool{}
	for _, h := range deps.Registry.ListAll() {
		armed[BareShortId(h.BundleId)] = true
	}
	if !armed[BareShortId(gated)] {
		t.Fatalf("the reconcile pass did not arm the gated bundle %s (%s); it would not show an ungated one either", gated, gatedAutomation)
	}
	for _, id := range ungated {
		if armed[BareShortId(id)] {
			t.Errorf("a reconcile pass armed %s, which no gate passed", id)
		}
	}
}

// camel joins a dashed test name into an identifier's tail: "raw-insert" ->
// "RawInsert".
func camel(name string) string {
	var b strings.Builder
	for _, part := range strings.Split(name, "-") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// rawBundleUpdate is the update() form at the write chokepoint -- what a
// mutation body's `update { }` reaches, a person's own authored mutation bound
// to the bundle concept included -- with no template's @serverOnly in front of
// it. Execute does not take an update(...) literal, so the form is driven
// through executeMutation, as the construct ladder's guard test drives it.
func rawBundleUpdate(t *testing.T, ctx context.Context, eng *MemQLEngine, id string, payload map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx = provenance.ContextWithProvenance(ctx, provenance.Direct("rawUpdate:"+bundleConceptID))
	_, err = eng.executeMutation(ctx, MutationNode{
		Kind:       ast.MutationKindUpdate,
		Concept:    bundleConceptID,
		ID:         id,
		PayloadRaw: string(raw),
	})
	return err
}

// bundleStatusRow reads a bundle row's latest status straight off the table;
// empty when there is no row.
func bundleStatusRow(t *testing.T, db *bun.DB, bundleId string) string {
	t.Helper()
	var status string
	err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(payload->>'status', '') FROM "MemoryNodes"
		 WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC LIMIT 1`, bundleConceptID, bundleId).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read bundle %s: %v", bundleId, err)
	}
	return status
}
