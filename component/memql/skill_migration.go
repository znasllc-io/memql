package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/provenance"
)

// MigrateAgentsToSkills runs the one-shot Phase 2 (memql#158) data
// migration: every v1:agents:agent row carrying the legacy
// capabilities.domains / capabilities.tools / capabilities.liveSources
// fields gets a fresh capabilities.skillIds[] derived from its role's
// lockedSkillIds + defaultSkillIds; a v1:agents:skillChangeEvent row
// records the attach attributed to system:migration:phase2.
//
// Gating: the engine calls this only when MEMQL_SKILL_MIGRATION_RUN
// is truthy (the env hook lives in app/integrations.go alongside the
// seed materializer wiring). Idempotent -- rows already on the new
// shape are skipped. Behind a flag so production rollouts can run
// the seed materializer first, then flip the flag to backfill once
// the operator confirms the role catalog landed cleanly.
//
// Pre-release reality: memQL has not shipped a production cluster
// yet, so the realistic input set for this migration is local dev
// databases + the CI fixture set. The function still ships the
// per-row transform so the moment we accumulate production data the
// migration path is in tree.
func (e *MemQLEngine) MigrateAgentsToSkills(ctx context.Context) (MigrationReport, error) {
	if e == nil {
		return MigrationReport{}, fmt.Errorf("skill migration: nil engine")
	}
	db := e.database()
	if db == nil {
		return MigrationReport{}, fmt.Errorf("skill migration: no database")
	}
	logger := e.Logger

	var agents []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&agents).
		Where("concept = ?", conceptAgentsAgent).
		OrderExpr(`"id" ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return MigrationReport{}, fmt.Errorf("scan agents: %w", err)
	}

	seen := map[string]struct{}{}
	report := MigrationReport{StartedAt: time.Now()}
	for _, node := range agents {
		if _, dup := seen[node.ID]; dup {
			continue // keep only the latest version per agent id
		}
		seen[node.ID] = struct{}{}

		var payload map[string]any
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			report.Errors = append(report.Errors,
				fmt.Sprintf("agent %s: payload decode: %v", node.ID, err))
			continue
		}
		caps, ok := payload["capabilities"].(map[string]any)
		if !ok || caps == nil {
			report.Skipped++
			continue
		}
		if existing := stringSliceFromAny(caps["skillIds"]); len(existing) > 0 {
			report.AlreadyMigrated++
			continue
		}
		legacyDomains := stringSliceFromAny(caps["domains"])
		legacyTools := stringSliceFromAny(caps["tools"])
		if len(legacyDomains) == 0 && len(legacyTools) == 0 {
			report.Skipped++
			continue
		}

		roleSlug := strings.TrimSpace(stringFromAny(payload["roleSlug"]))
		newSkillIds, err := e.deriveSkillIdsForLegacyAgent(ctx, roleSlug, node.ID)
		if err != nil {
			report.Errors = append(report.Errors,
				fmt.Sprintf("agent %s: derive skills: %v", node.ID, err))
			continue
		}
		if len(newSkillIds) == 0 {
			report.Unmappable++
			if logger != nil {
				logger.Warn("skill migration: agent has no derivable skills (manual cleanup needed)",
					"agentId", node.ID, "roleSlug", roleSlug,
					"legacyDomains", len(legacyDomains),
					"legacyTools", len(legacyTools))
			}
			continue
		}

		caps["skillIds"] = newSkillIds
		delete(caps, "domains")
		delete(caps, "tools")
		delete(caps, "liveSources")
		payload["capabilities"] = caps

		before := map[string]any{
			"domains":     legacyDomains,
			"tools":       legacyTools,
			"liveSources": stringSliceFromAny(caps["liveSources"]),
		}
		after := map[string]any{"skillIds": newSkillIds}

		if err := e.writeMigratedAgent(ctx, node.ID, payload); err != nil {
			report.Errors = append(report.Errors,
				fmt.Sprintf("agent %s: update: %v", node.ID, err))
			continue
		}
		for _, sid := range newSkillIds {
			if err := e.writeSkillChangeEvent(ctx, node.ID, sid, before, after); err != nil {
				report.Errors = append(report.Errors,
					fmt.Sprintf("agent %s: skillChangeEvent %s: %v", node.ID, sid, err))
			}
		}
		report.Migrated++
	}

	report.CompletedAt = time.Now()
	if logger != nil {
		logger.Info("skill migration: complete",
			"migrated", report.Migrated,
			"alreadyMigrated", report.AlreadyMigrated,
			"unmappable", report.Unmappable,
			"skipped", report.Skipped,
			"errors", len(report.Errors),
			"durationMs", report.CompletedAt.Sub(report.StartedAt).Milliseconds())
	}
	return report, nil
}

// MigrationReport summarises a one-shot run of MigrateAgentsToSkills.
// Surfaced to the operator via the launcher's stdout + structured
// log entry; archived on the migration-history concept once that
// surface ships.
type MigrationReport struct {
	StartedAt       time.Time
	CompletedAt     time.Time
	Migrated        int
	AlreadyMigrated int
	Unmappable      int
	Skipped         int
	Errors          []string
}

// deriveSkillIdsForLegacyAgent picks the minimum-cover skill set for
// a legacy agent. v1 strategy: fall back to the agent's role's
// lockedSkillIds (which the role catalog re-seeded with the new
// shape carries already). Returns an empty slice when the role
// doesn't exist or carries no locked skills -- the caller treats
// that as "unmappable, log + skip" so a buggy role catalog can't
// silently corrupt agent rows.
func (e *MemQLEngine) deriveSkillIdsForLegacyAgent(ctx context.Context, roleSlug, agentId string) ([]string, error) {
	if roleSlug == "" {
		return nil, nil
	}
	role, err := e.fetchAgentRole(ctx, roleSlug)
	if err != nil {
		return nil, err
	}
	if role == nil || len(role.lockedSkillIds) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(role.lockedSkillIds))
	seen := map[string]struct{}{}
	for _, sid := range role.lockedSkillIds {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out, nil
}

// writeMigratedAgent issues mutationUpdateAgent with the rewritten
// payload. The lock validator runs server-side; a malformed rewrite
// rejects with the same lock messages a runtime caller would see.
func (e *MemQLEngine) writeMigratedAgent(ctx context.Context, agentId string, payload map[string]any) error {
	args := map[string]any{
		"agentId": agentId,
		"payload": payload,
	}
	payloadJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	call := fmt.Sprintf(`mutationUpdateAgent(%s)`, string(payloadJSON))
	ctx = provenance.ContextWithProvenance(systemActorContext(ctx), provenance.System("migration:phase2:skill-cut"))
	_, err = e.Execute(ctx, call)
	return err
}

// writeSkillChangeEvent stamps the per-attach audit row mirroring
// the Phase 2 contract: changeKind='attached', actorUserId=
// 'system:migration:phase2', planId empty.
func (e *MemQLEngine) writeSkillChangeEvent(ctx context.Context, agentId, skillId string, before, after map[string]any) error {
	args := map[string]any{
		"skillChangeEventId": fmt.Sprintf("migration-%s-%s-%d", agentId, skillId, time.Now().UnixNano()),
		"targetAgentId":      agentId,
		"skillId":            skillId,
		"changeKind":         "attached",
		"before":             before,
		"after":              after,
		"actorUserId":        "system:migration:phase2",
	}
	payloadJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal event args: %w", err)
	}
	call := fmt.Sprintf(`mutationCreateSkillChangeEvent(%s)`, string(payloadJSON))
	ctx = provenance.ContextWithProvenance(systemActorContext(ctx), provenance.System("migration:phase2:skill-cut"))
	_, err = e.Execute(ctx, call)
	return err
}

