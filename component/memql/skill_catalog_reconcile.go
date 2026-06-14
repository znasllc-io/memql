package memql

import (
	"context"
	"fmt"
	"sort"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// skill_catalog_reconcile.go -- boot-time, system-managed reconcile that
// guarantees every registered `seed skill` declaration has a materialized
// v1:agents:skill catalog row (memql#1459).
//
// Why this exists on top of the global materialization pass:
//
// The startup sweep (SeedMaterializer.Start) already materializes every
// global seed -- including skills -- by walking m.registry. That registry
// is overlay-inclusive: carrier-`RegisterTree`'d DSL subtrees (the
// CoPresent BFF's dsl/copresent/skills.memql, mounted via
// dsl.RegisterTree("copresent", ...)) flow through LoadUnifiedSeeds into
// the SAME registry, so on a carrier node the global pass DOES attempt to
// write the copresent-takeover / copresent-guide / copresent-ui /
// copresent-canvas rows. (This is provable in-process; see
// TestSeedMaterializer_CarrierOverlaySkillRowsResolve.)
//
// The staging gap #1459 documents is a DATA gap, not a code-path gap: the
// per-agent voice scope read (resolveAgentToolSlugsVia -> ResolveSkills)
// resolves the Assistant's capabilities.skillIds (which include the
// copresent skills) against v1:agents:skill ROWS -- and on staging those
// rows were absent, so ResolveSkills silently dropped them
// ("unknown skill id (skipped)") and the realtime model got zero operator
// primitives (uiClick/uiType/...). The assistant-skillId reconcile (#1443)
// backfills the *references* on existing assistant rows, but it cannot fix
// a missing *catalog* row those references point to.
//
// This reconcile closes that loop the same way #1443 closed the assistant
// loop: it is boot-time, idempotent, and merge-only (it only creates rows
// that are MISSING; it never rewrites an existing skill row, so a
// user-/admin-edited catalog row is preserved). It self-heals the staging
// gap on the next carrier-node boot -- without it, a node whose earlier
// sweep failed to land a particular skill row never re-attempts that row
// because the global pass treats a per-row failure as logged-and-continue
// and does not re-verify on subsequent boots within the same row's
// lifetime.

// SkillCatalogReconcileReport summarizes a reconcileSkillCatalog run.
// Returned for tests + boot logging.
type SkillCatalogReconcileReport struct {
	// Registered is the number of global `seed skill` defs in the
	// registry (the universe we reconcile against).
	Registered int
	// AlreadyOK is the number whose v1:agents:skill row already existed.
	AlreadyOK int
	// Materialized is the number whose row was MISSING and got written.
	Materialized int
	// Errors collects per-skill failures (logged; non-fatal).
	Errors []string
}

// reconcileSkillCatalog ensures every registered global `seed skill`
// declaration has a materialized v1:agents:skill row. It scans the
// existing skill rows once, then re-materializes only the skills whose
// row is absent. Idempotent + create-only: existing rows are left
// untouched.
//
// Runs after the global materialization pass (so a freshly-materialized
// row counts as "already OK") and is best-effort: a failure is logged and
// boot continues, exactly like reconcileAssistantSkills (#1443).
func (m *SeedMaterializer) reconcileSkillCatalog(ctx context.Context) (SkillCatalogReconcileReport, error) {
	report := SkillCatalogReconcileReport{}
	if m == nil || m.engine == nil || m.registry == nil {
		return report, fmt.Errorf("skill catalog reconcile: nil engine/registry")
	}
	logger := m.engine.Logger

	// Collect the registered global skill seeds. The discriminator is
	// UseConcept=="skill": the canonical authoring form
	// `use agents.concepts.{ skill }` binds UseConcept but leaves
	// UseNamespace empty (the concept registry resolves "skill" ->
	// "v1:agents:skill" without the bare namespace being threaded onto
	// the SeedDefinition), so filtering on UseNamespace would match
	// nothing. perUser skills don't exist today, but the scope guard
	// keeps the contract explicit.
	var skillSeeds []*SeedDefinition
	for _, def := range m.registry.All() {
		if def == nil {
			continue
		}
		if def.UseConcept != "skill" {
			continue
		}
		if def.Scope != "global" {
			continue
		}
		skillSeeds = append(skillSeeds, def)
	}
	report.Registered = len(skillSeeds)
	if len(skillSeeds) == 0 {
		if logger != nil {
			logger.Info("skill catalog reconcile: no global skill seeds registered; skipping")
		}
		return report, nil
	}

	db := m.engine.database()
	if db == nil {
		return report, fmt.Errorf("skill catalog reconcile: no database")
	}

	// One scan of the catalog: build the set of skill ids that already
	// have a row. memql is time-series; the presence of ANY version is
	// enough to mark a skill as materialized.
	var rows []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&rows).
		Where("concept = ?", conceptAgentsSkill).
		Column("id").
		Scan(ctx); err != nil {
		return report, fmt.Errorf("skill catalog reconcile: scan skills: %w", err)
	}
	present := make(map[string]struct{}, len(rows))
	for _, node := range rows {
		present[node.ID] = struct{}{}
	}

	// Sort for deterministic boot logs.
	sort.Slice(skillSeeds, func(i, j int) bool {
		return skillSeeds[i].Name < skillSeeds[j].Name
	})

	var missing []string
	for _, def := range skillSeeds {
		// The global skill seed's row id is the canonical
		// v1:agents:skill:<seedName> (compileSeedDecl auto-derives the
		// body `id` from the seed name; materializeGlobal stamps it into
		// skillId, and the row lands at the canonical concept-prefixed id).
		rowId := conceptAgentsSkill + ":" + def.Name
		if _, ok := present[rowId]; ok {
			report.AlreadyOK++
			continue
		}
		// Row missing -- re-materialize it through the SAME global path
		// the startup sweep uses (mutationCreateSkill via the seed body).
		// provenance.Seed mirrors the original materialization so the
		// re-written row is indistinguishable from a first-sweep write.
		if err := m.materializeGlobal(ctx, def); err != nil {
			report.Errors = append(report.Errors,
				fmt.Sprintf("skill %s: %v", def.Name, err))
			continue
		}
		report.Materialized++
		missing = append(missing, def.Name)
	}

	if logger != nil {
		if report.Materialized > 0 {
			logger.Warn("skill catalog reconcile: backfilled MISSING skill rows",
				"materialized", report.Materialized,
				"skills", missing,
				"registered", report.Registered,
				"alreadyOK", report.AlreadyOK)
		}
		logger.Info("skill catalog reconcile: complete",
			"registered", report.Registered,
			"alreadyOK", report.AlreadyOK,
			"materialized", report.Materialized,
			"errors", len(report.Errors))
	}
	return report, nil
}
