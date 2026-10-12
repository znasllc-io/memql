package app

// planner_construct_writer_db_test.go -- the planner's member-construct writes
// reach the database through the App's planner engine (memql#6022).
//
// createAuthoringConstruct is @serverOnly and integrations/planner may not stamp
// internal origin, so the planner hands the write to the engine's writer
// through CognitionEngineAdapter. A planner build asserts the adapter has the
// method (integrations_planner_init.go); this asserts the method WRITES, against
// a real Postgres, and that the write guard still holds it to a draft bundle.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func TestCognitionEngineAdapterWritesAuthoredConstructs(t *testing.T) {
	engine, db := workTemplateDBEngineAndDB(t)
	adapter := &CognitionEngineAdapter{Engine: engine}
	owner := id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	bundle := "v1:authoring:bundle:" + id.NewShortId()
	templateMutation(t, engine, ctx, "createAuthoringBundle", map[string]any{"bundleId": bundle, "title": "Planner draft"})

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

	// Once the bundle's gates have run, the same writer is refused.
	templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "recordBundleDryRun", map[string]any{"bundleId": bundle, "status": "dryRunPassed", "dryRunReport": map[string]any{"ok": true}})
	if err := adapter.WriteAuthoredConstruct(ctx, memql.AuthoredConstructInput{
		ConstructId: "v1:authoring:construct:" + id.NewShortId(), BundleId: bundle, Kind: "automation", Name: "tooLate",
		TargetNamespace: "authored", Source: "automation tooLate { }",
	}); err == nil {
		t.Fatalf("a construct joined bundle %s after its dry run", bundle)
	}
}
