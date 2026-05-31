package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptAgentsSkill is the canonical concept id the resolver queries
// against. Local to this file for the same reason as
// conceptAgentsAgent / conceptAgentsAgentRole in agent_lock_validation.go.
const conceptAgentsSkill = "v1:agents:skill"

// SkillBundle is the resolved effective surface for a list of skill
// ids. domains / toolSlugs / liveSourceIds are deduped + lexically
// sorted so call sites get stable iteration order.
type SkillBundle struct {
	DomainIds     []string
	ToolSlugs     []string
	LiveSourceIds []string
}

// ResolveSkills returns the unioned domain / tool / live-source
// surface for a list of skill ids. The empty input yields an empty
// bundle. Unknown skill ids are warn-logged and skipped (the lock
// validator already gates write-time references; read-time is
// best-effort to keep retrieval / scoring resilient).
//
// Phase 2 (#158) is the core helper every consumer of the new agent
// shape calls before reading capabilities: replier resolution, the
// cognition fit-score, the conductor's per-agent block, the agent
// factory's createSpecialist / extendSpecialist mutations.
func (e *MemQLEngine) ResolveSkills(ctx context.Context, skillIds []string) (SkillBundle, error) {
	if e == nil || len(skillIds) == 0 {
		return SkillBundle{}, nil
	}
	db := e.database()
	if db == nil {
		return SkillBundle{}, nil
	}

	// Dedupe + sort the input so the cache key (when we add one) is
	// stable and the DB query's IN clause is deterministic.
	//
	// Callers pass bare slugs (`copresent-takeover`) by convention --
	// matches what role catalog rows store in lockedSkillIds /
	// defaultSkillIds / availableSkillIds / forbiddenSkillIds and what
	// the agent seeds write into capabilities.skillIds. The catalog
	// rows themselves are keyed on the canonical id form
	// (`v1:agents:skill:copresent-takeover`), so we prepend the concept
	// prefix here when an input doesn't already carry one. Without this
	// normalization the `id IN (?)` query never matches and every
	// resolver call returns an empty bundle. memql#376.
	wantSet := map[string]struct{}{}
	want := make([]string, 0, len(skillIds))
	for _, id := range skillIds {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !strings.HasPrefix(id, conceptAgentsSkill+":") {
			id = conceptAgentsSkill + ":" + id
		}
		if _, dup := wantSet[id]; dup {
			continue
		}
		wantSet[id] = struct{}{}
		want = append(want, id)
	}
	if len(want) == 0 {
		return SkillBundle{}, nil
	}
	sort.Strings(want)

	// memql is time-series; we want the latest version of each row.
	// Pull all rows for the requested ids, group by id, keep the
	// newest createdAt per id. The catalog is read-mostly + small
	// (currently 25 skills), so a SELECT-without-pagination is fine.
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptAgentsSkill).
		Where("id IN (?)", bun.In(want)).
		OrderExpr(`"id" ASC, "createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return SkillBundle{}, fmt.Errorf("scan v1:agents:skill: %w", err)
		}
	}

	domains := map[string]struct{}{}
	tools := map[string]struct{}{}
	liveSources := map[string]struct{}{}
	seen := map[string]struct{}{}
	for _, node := range nodes {
		if _, dup := seen[node.ID]; dup {
			continue // keep the first (newest) row per id
		}
		seen[node.ID] = struct{}{}
		var payload map[string]any
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			if e.Logger != nil {
				e.Logger.Warn("ResolveSkills: skill payload decode failed",
					"skillId", node.ID, "error", err)
			}
			continue
		}
		for _, d := range stringSliceFromAny(payload["domainIds"]) {
			if d != "" {
				domains[d] = struct{}{}
			}
		}
		for _, t := range stringSliceFromAny(payload["toolSlugs"]) {
			if t != "" {
				tools[t] = struct{}{}
			}
		}
		for _, l := range stringSliceFromAny(payload["liveSourceIds"]) {
			if l != "" {
				liveSources[l] = struct{}{}
			}
		}
	}

	if e.Logger != nil {
		for _, id := range want {
			if _, ok := seen[id]; !ok {
				e.Logger.Warn("ResolveSkills: unknown skill id (skipped)", "skillId", id)
			}
		}
	}

	return SkillBundle{
		DomainIds:     sortedKeys(domains),
		ToolSlugs:     sortedKeys(tools),
		LiveSourceIds: sortedKeys(liveSources),
	}, nil
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
