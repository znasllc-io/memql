package memql

import (
	"context"
	"encoding/json"
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
	// AlreadyOK is the number whose v1:agents:skill row already existed
	// AND whose persisted capability lists (toolSlugs / domainIds /
	// liveSourceIds) already match the registered seed.
	AlreadyOK int
	// Materialized is the number whose row was MISSING and got written.
	Materialized int
	// Healed is the number whose row existed but carried STALE/empty
	// capability lists (diverging from the registered seed) and got
	// re-materialized to the canonical surface (#1462).
	Healed int
	// Errors collects per-skill failures (logged; non-fatal).
	Errors []string
}

// reconcileSkillCatalog ensures every registered global `seed skill`
// declaration has a materialized v1:agents:skill row WHOSE capability
// lists match the registered seed. It scans the existing skill rows
// once (id + payload), then re-materializes the skills whose row is
// absent OR whose persisted toolSlugs / domainIds / liveSourceIds
// DIVERGE from the seed (the stale / half-written case, #1462).
// Idempotent: a row that already matches is left untouched.
//
// Why divergence-healing and not just missing-row backfill (#1462):
// the carrier capability-slug skills (copresent-takeover et al., whose
// toolSlugs are their OWN slug, fanned out by ExpandCapabilitySlugs)
// shipped under an earlier shape on staging -- copresent-takeover was
// once copresent-control with toolSlugs ["copresent-control"] before
// the split (memql-bff-copresent #187 / memql#376). A row materialized
// under the old shape (or half-written with empty toolSlugs) still
// EXISTS by id, so the original missing-only reconcile reported it as
// alreadyOK and never rewrote it -- ResolveSkills then read the stale /
// empty toolSlugs, copresent-takeover never reached ExpandCapabilitySlugs,
// and the realtime voice scope resolved zero UI primitives (the #1462
// staging symptom: exposed_count=19, all core skills). Comparing the
// persisted lists against the registered seed self-heals that on the
// next carrier-node boot. memql is time-series: the re-materialize
// appends a fresh, correct version and every reader (ResolveSkills
// keeps the newest per id) immediately sees the canonical surface.
//
// Predefined-only: divergence-healing applies only to seed-managed
// (predefined) catalog rows. A user-/admin-edited row is intentionally
// left alone (the seed never claimed authority over it). Carrier skill
// seeds are all predefined:true, so this covers the failure class
// without churning hand-curated rows.
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

	// One scan of the catalog: pull id + payload + createdAt for every
	// skill row, then keep the NEWEST version per id (memql is
	// time-series). We need the payload -- not just presence -- so we
	// can detect a row whose persisted capability lists have gone stale
	// vs the registered seed (#1462).
	var rows []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&rows).
		Where("concept = ?", conceptAgentsSkill).
		Column("id", "payload").
		OrderExpr(`"id" ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return report, fmt.Errorf("skill catalog reconcile: scan skills: %w", err)
	}
	// present maps row id -> the newest persisted payload (nil if the
	// payload failed to decode -- treated as a divergence to be safe).
	present := make(map[string]map[string]any, len(rows))
	for _, node := range rows {
		if _, seen := present[node.ID]; seen {
			continue // first row per id is newest (ORDER BY createdAt DESC)
		}
		var payload map[string]any
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			present[node.ID] = nil
			continue
		}
		present[node.ID] = payload
	}

	// Sort for deterministic boot logs.
	sort.Slice(skillSeeds, func(i, j int) bool {
		return skillSeeds[i].Name < skillSeeds[j].Name
	})

	var missing []string
	var healed []string
	for _, def := range skillSeeds {
		// The global skill seed's row id is the canonical
		// v1:agents:skill:<seedName> (compileSeedDecl auto-derives the
		// body `id` from the seed name; materializeGlobal stamps it into
		// skillId, and the row lands at the canonical concept-prefixed id).
		rowId := conceptAgentsSkill + ":" + def.Name
		payload, ok := present[rowId]
		switch {
		case !ok:
			// Row missing -- re-materialize it through the SAME global path
			// the startup sweep uses (createSkill via the seed body).
			// provenance.Seed mirrors the original materialization so the
			// re-written row is indistinguishable from a first-sweep write.
			if err := m.materializeGlobal(ctx, def); err != nil {
				report.Errors = append(report.Errors,
					fmt.Sprintf("skill %s (missing): %v", def.Name, err))
				continue
			}
			report.Materialized++
			missing = append(missing, def.Name)
		case skillRowStale(def, payload):
			// Row exists but its persisted capability lists diverge from
			// the registered (predefined) seed -- the staging stale-row
			// case (#1462). Re-materialize to append a corrected version;
			// memql's time-series read keeps the newest, so ResolveSkills
			// immediately sees the canonical toolSlugs / domainIds /
			// liveSourceIds.
			if err := m.materializeGlobal(ctx, def); err != nil {
				report.Errors = append(report.Errors,
					fmt.Sprintf("skill %s (stale): %v", def.Name, err))
				continue
			}
			report.Healed++
			healed = append(healed, def.Name)
		default:
			report.AlreadyOK++
		}
	}

	if logger != nil {
		if report.Materialized > 0 {
			logger.Warn("skill catalog reconcile: backfilled MISSING skill rows",
				"materialized", report.Materialized,
				"skills", missing,
				"registered", report.Registered,
				"alreadyOK", report.AlreadyOK)
		}
		if report.Healed > 0 {
			logger.Warn("skill catalog reconcile: healed STALE skill rows (capability lists diverged from seed)",
				"healed", report.Healed,
				"skills", healed,
				"registered", report.Registered,
				"alreadyOK", report.AlreadyOK)
		}
		logger.Info("skill catalog reconcile: complete",
			"registered", report.Registered,
			"alreadyOK", report.AlreadyOK,
			"materialized", report.Materialized,
			"healed", report.Healed,
			"errors", len(report.Errors))
	}
	return report, nil
}

// skillRowStale reports whether a persisted skill row's capability lists
// (toolSlugs / domainIds / liveSourceIds) diverge from what the
// registered seed declares -- the signal that a row materialized under
// an older seed shape (or half-written with empty lists) needs
// re-materialization (#1462).
//
// Only predefined (seed-managed) rows are eligible: a user-/admin-edited
// row is intentionally never healed (the seed never owned it). Carrier
// skill seeds are all predefined:true. A row whose payload failed to
// decode (payload==nil) is treated as stale so a corrupt row self-heals.
//
// The comparison is order-insensitive and set-based: the seed authoring
// order and the persisted order need not match, and a re-materialize
// that only reorders the list is NOT a divergence (avoids a perpetual
// re-heal loop). Pure (no engine / no IO) so it's unit-testable.
func skillRowStale(def *SeedDefinition, payload map[string]any) bool {
	if payload == nil {
		return true
	}
	// Respect non-predefined rows: a user-edited catalog row is off-limits.
	if pre, ok := payload["predefined"].(bool); ok && !pre {
		return false
	}
	for _, field := range []string{"toolSlugs", "domainIds", "liveSourceIds"} {
		want := seedListField(def, field)
		got := stringSliceFromAny(payload[field])
		if !sameStringSet(want, got) {
			return true
		}
	}
	return false
}

// seedListField pulls a string-array field off a seed body, returning the
// declared slugs (empty when the field is absent or not a string array).
func seedListField(def *SeedDefinition, field string) []string {
	if def == nil {
		return nil
	}
	v, ok := def.Body.fields[field]
	if !ok || v.kind != seedStringArray {
		return nil
	}
	return append([]string(nil), v.stringsV...)
}

// sameStringSet reports whether a and b contain the same set of strings
// (order- and duplicate-insensitive). Empty-vs-empty is equal.
func sameStringSet(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	seen := make(map[string]struct{}, len(b))
	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
		seen[s] = struct{}{}
	}
	return len(seen) == len(set)
}
