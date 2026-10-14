package app

// planner_construct_writer_db_test.go -- the planner's bundle, member-construct
// and Gate 1 verdict writes reach the database through the App's planner engine
// (memql#6022).
//
// createAuthoringBundle, createAuthoringConstruct and recordBundleValidation
// are @serverOnly and integrations/planner may not stamp internal origin, so
// the planner hands the writes to the engine's writers through
// CognitionEngineAdapter. A planner build asserts the adapter has the methods
// (integrations_planner_init.go); this asserts the methods WRITE, against a
// real Postgres, and that the write guard still holds a member to a draft
// bundle.

import (
	"context"
	"testing"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func testCognitionEngineAdapterWritesAuthoringBundlesAndConstructs(t *testing.T, engine *memql.MemQLEngine, db *bun.DB) {
	adapter := &CognitionEngineAdapter{Engine: engine}
	owner := id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	bundle := "v1:authoring:bundle:" + id.NewShortId()
	if err := adapter.WriteAuthoringBundle(ctx, memql.AuthoringBundleInput{BundleId: bundle, Title: "Planner draft"}); err != nil {
		t.Fatalf("the planner's bundle: %v", err)
	}
	var stored string
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(payload->>'ownerUserId', '') FROM "MemoryNodes" WHERE concept = 'v1:authoring:bundle' AND id = ? ORDER BY "createdAt" DESC LIMIT 1`, bundle).Scan(&stored); err != nil ||
		memql.BareShortId(stored) != owner {
		t.Fatalf("the planner's bundle did not land as the owner's (owner %q, %v)", stored, err)
	}

	construct := "v1:authoring:construct:" + id.NewShortId()
	if err := adapter.WriteAuthoredConstruct(ctx, memql.AuthoredConstructInput{
		ConstructId: construct, BundleId: bundle, Kind: "automation", Name: "plannerDraft",
		TargetNamespace: "authored", Source: "automation plannerDraft { }",
	}); err != nil {
		t.Fatalf("the planner's construct write into its draft bundle: %v", err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:authoring:construct' AND id = ?`, construct).Scan(&n); err != nil || n == 0 {
		t.Fatalf("the planner's construct did not land (%d rows, %v)", n, err)
	}

	// Gate 1's verdict, through the adapter's recorder (recordBundleValidation
	// is @serverOnly too).
	if err := adapter.RecordBundleValidation(ctx, memql.BundleValidationInput{
		BundleId: bundle, Status: "validated", Report: map[string]any{"ok": true},
	}); err != nil {
		t.Fatalf("the planner's Gate 1 verdict: %v", err)
	}
	var status string
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(payload->>'status', '') FROM "MemoryNodes" WHERE concept = 'v1:authoring:bundle' AND id = ? ORDER BY "createdAt" DESC LIMIT 1`, bundle).Scan(&status); err != nil ||
		status != "validated" {
		t.Fatalf("the planner's Gate 1 verdict left the bundle %q (%v), want validated", status, err)
	}

	// Once the bundle's gates have run, the same writer is refused.
	templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "recordBundleDryRun", map[string]any{"bundleId": bundle, "status": "dryRunPassed", "dryRunReport": map[string]any{"ok": true}})
	if err := adapter.WriteAuthoredConstruct(ctx, memql.AuthoredConstructInput{
		ConstructId: "v1:authoring:construct:" + id.NewShortId(), BundleId: bundle, Kind: "automation", Name: "tooLate",
		TargetNamespace: "authored", Source: "automation tooLate { }",
	}); err == nil {
		t.Fatalf("a construct joined bundle %s after its dry run", bundle)
	}
}
