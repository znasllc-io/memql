package memql

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// skill_catalog_reconcile_db_test.go is the MANDATORY real-engine
// reproduction + fix guard for memql#1459.
//
// It boots a REAL embedded-DSL engine against a REAL Postgres (the same
// New + Init path app.Run runs), mounts a CoPresent-flavored skill the
// exact way the carrier (memql-bff-copresent) does -- via
// dsl.RegisterTree("<domain>", overlayFS) at boot, which is how the
// copresent-takeover / -guide / -ui / -canvas skills reach the engine --
// and then drives the full staging failure mode:
//
//	agent.capabilities.skillIds -> ResolveSkills (a v1:agents:skill ROW
//	lookup) -> ExpandCapabilitySlugs (copresent-takeover -> uiClick/uiType/...)
//
// The bug (#1459): on staging the v1:agents:skill rows for the copresent
// skills were ABSENT, so ResolveSkills resolved them to NOTHING and the
// realtime voice model got zero UI primitives. NO MOCKS: the #1455 test
// passed only because it hand-fed a canned skill->tool map; here the tool
// surface comes from a row ResolveSkills reads off the live catalog.
//
// Reproduction shape (so the test FAILS pre-fix and PASSES post-fix
// without depending on whether the global pass happens to have written the
// row): we register the overlay skill, run the FULL materializer sweep,
// then DELETE the catalog row to simulate the staging data gap, assert
// ResolveSkills is empty (the bug), then run reconcileSkillCatalog (the
// fix) and assert ResolveSkills now resolves + ExpandCapabilitySlugs
// yields uiClick/uiType. Pre-fix the reconcile method does not exist /
// is not wired, so the row stays missing and the post-reconcile
// assertions fail.
//
// Postgres-gated: skips when no DB is reachable (CI without a DB), so it
// never blocks the pipeline; it runs locally against the docker `memql-db`
// and on any node with MEMORY_NODES_DATABASE_DSN set.

func reconcileTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMORY_NODES_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_local_dev@localhost:5432/memql?sslmode=disable"
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("skill catalog reconcile DB test: no Postgres reachable (%v)", err)
	}
	return db
}

// carrierSkillOverlay mirrors the carrier's dsl/copresent/skills.memql
// shape for one operator skill: a global `seed skill` whose toolSlugs
// carry the "copresent-takeover" capability slug that ExpandCapabilitySlugs
// fans out to the operator primitives. Mounted via RegisterTree exactly as
// the carrier mounts its copresent domain.
func carrierSkillOverlay(slug string) fstest.MapFS {
	body := `use agents.concepts.{ skill }

seed skill ` + slug + ` {
  slug:          "` + slug + `"
  name:          "Reconcile Probe Takeover"
  description:   "carrier-overlay operator skill for the #1459 reconcile guard"
  category:      "product"
  tags:          ["copresent", "operator"]
  domainIds:     ["copresent-ui"]
  toolSlugs:     ["copresent-takeover"]
  liveSourceIds: []
  tier:          "A"
  predefined:    true
}
`
	return fstest.MapFS{"skills.memql": &fstest.MapFile{Data: []byte(body)}}
}

func TestSeedMaterializer_CarrierOverlaySkillRowsResolve(t *testing.T) {
	const slug = "reconcile-probe-takeover"
	const domain = "reconcileprobe"
	rowId := conceptAgentsSkill + ":" + slug

	// Mount the carrier-style overlay the SAME way the bff carrier does.
	memqldsl.RegisterTree(domain, carrierSkillOverlay(slug))

	db := reconcileTestDB(t)
	ctx := context.Background()
	// Clean any residue from a prior local run so the test is hermetic.
	_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
		Where("concept = ?", conceptAgentsSkill).Where("id = ?", rowId).Exec(ctx)

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("Init: %v", err)
	}

	sm := eng.SeedMaterializer()
	if sm == nil {
		t.Fatal("nil seed materializer")
	}

	// Confirm the overlay seed reached the registry the materializer
	// iterates -- i.e. the carrier RegisterTree path IS included (the
	// issue's hypothesis that the registry excludes overlay seeds is
	// false; the real bug is the missing ROW).
	if _, ok := eng.Seeds().Get(slug); !ok {
		t.Fatalf("overlay seed %q absent from the materializer registry -- RegisterTree path broken", slug)
	}

	// Run the full sweep (writes the row on a carrier node), then DELETE
	// the catalog row to reproduce the staging data gap: the assistant's
	// skillIds still reference it, but the v1:agents:skill row is gone.
	if err := sm.Start(ctx); err != nil {
		t.Fatalf("materializer Start: %v", err)
	}
	if _, err := db.NewDelete().Model((*concept.MemoryNode)(nil)).
		Where("concept = ?", conceptAgentsSkill).Where("id = ?", rowId).Exec(ctx); err != nil {
		t.Fatalf("simulate staging gap (delete skill row): %v", err)
	}

	// --- BUG: with the row absent, ResolveSkills drops the skill -------
	gapBundle, err := eng.ResolveSkills(ctx, []string{slug})
	if err != nil {
		t.Fatalf("ResolveSkills (gap): %v", err)
	}
	if len(gapBundle.ToolSlugs) != 0 {
		t.Fatalf("expected empty resolve with the catalog row missing (the #1459 bug); got %v", gapBundle.ToolSlugs)
	}
	t.Logf("#1459 reproduced: ResolveSkills(%q) with the row missing -> empty (model gets zero UI primitives)", slug)

	// --- FIX: reconcileSkillCatalog re-materializes the missing row ----
	report, err := sm.reconcileSkillCatalog(ctx)
	if err != nil {
		t.Fatalf("reconcileSkillCatalog: %v", err)
	}
	if report.Materialized < 1 {
		t.Fatalf("reconcile should have re-materialized the missing skill row; report=%+v", report)
	}

	// After the fix the full chain resolves: ResolveSkills returns the
	// toolSlug, and ExpandCapabilitySlugs fans copresent-takeover out to
	// the operator primitives (uiClick / uiType / ...).
	fixedBundle, err := eng.ResolveSkills(ctx, []string{slug})
	if err != nil {
		t.Fatalf("ResolveSkills (fixed): %v", err)
	}
	expanded := ExpandCapabilitySlugs(fixedBundle.ToolSlugs)
	want := []string{"uiClick", "uiType", "uiNavigate"}
	got := map[string]struct{}{}
	for _, tn := range expanded {
		got[tn] = struct{}{}
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Fatalf("post-fix expanded tool set must contain %q; raw=%v expanded=%v", w, fixedBundle.ToolSlugs, expanded)
		}
	}
	t.Logf("#1459 fixed: ResolveSkills(%q) raw=%v -> expanded(%d)=%v", slug, fixedBundle.ToolSlugs, len(expanded), expanded)

	// Idempotency: a second reconcile materializes nothing new.
	report2, err := sm.reconcileSkillCatalog(ctx)
	if err != nil {
		t.Fatalf("reconcileSkillCatalog (2nd): %v", err)
	}
	if report2.Materialized != 0 {
		t.Fatalf("reconcile must be idempotent; 2nd run materialized %d (report=%+v)", report2.Materialized, report2)
	}

	// Cleanup.
	_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
		Where("concept = ?", conceptAgentsSkill).Where("id = ?", rowId).Exec(ctx)
}
