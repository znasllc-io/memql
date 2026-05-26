package main

import (
	"strings"
)

// AgentRow is the minimal projection of v1:agents:agent (or any
// perUser-seeded concept) needed to plan the dedupe. One row per
// distinct id (the caller already collapses time-series versions).
type AgentRow struct {
	ID          string // full canonical id, e.g. "v1:agents:agent:272efbaa-..."
	Concept     string // e.g. "v1:agents:agent"
	OwnerUserID string // full canonical user id, e.g. "v1:identity:user:395e4e72-..."
	SeedName    string // provenance.name, e.g. "assistant"
}

// ParticipantRow is the minimal projection of v1:cognition:participant
// needed to identify orphans pointing at doomed agents.
type ParticipantRow struct {
	ID      string // full canonical participant id
	AgentID string // full canonical agent id the participant points at
}

// Plan is the precomputed work a migration run will do, separable
// from any DB I/O so the planning logic is unit-testable.
type Plan struct {
	// Per-seed, per-owner detail. Useful for human-readable
	// dry-run output and for the post-execute summary.
	Groups []Group

	// Flattened delete lists. The migration's transaction issues
	// DELETE FROM "MemoryNodes" WHERE id = ANY($1) twice -- once for
	// agents, once for participants. Hard delete is intentional:
	// the duplicates were never legitimate data, and memql's
	// (id, createdAt) PK means we have to scrub every version
	// anyway.
	DoomedAgentIDs       []string
	DoomedParticipantIDs []string
}

// Group is the per-(seed, owner) audit unit. Keep = the row whose
// id matches the deterministic `<seedName>-<bareUserId>` contract
// (zero or one entries; see PR #274). Doomed = everything else for
// that pair. NeedsReseed is set when Keep is empty -- the migration
// removed every row for this (seed, owner) and the materializer's
// startup sweep must run again to lay down the canonical id.
type Group struct {
	SeedName    string
	OwnerUserID string
	ExpectedID  string // full canonical id the keeper SHOULD have
	Keep        []string
	Doomed      []string
	NeedsReseed bool
}

// ComputePlan partitions the input agent + participant rows into
// (keep, doom) per (seedName, ownerUserId) group. The contract:
//
//   - The canonical id is `<concept>:<seedName>-<bareUserId>` where
//     bareUserId is the userId with the `v1:identity:user:` prefix
//     stripped. This mirrors deterministicPerUserSeedId in
//     component/memql/seed_materializer.go (PR #274).
//   - For each group, rows whose id matches the canonical form are
//     kept; everything else is doomed.
//   - Participants pointing at a doomed agent id are also doomed.
//   - Groups where Keep is empty (no row matches the canonical
//     form -- the common case before any cluster boot post-#274)
//     surface NeedsReseed=true so the runbook can prompt for a
//     cluster restart.
//
// Pure function. No DB access; the caller handles SQL.
func ComputePlan(agents []AgentRow, participants []ParticipantRow) Plan {
	// Bucket agents by (seedName, ownerUserId) so the per-group
	// audit detail is preserved. A map keyed by struct is the
	// straightforward shape -- groups stay distinct without
	// hashing or concatenation tricks.
	type key struct {
		seedName    string
		ownerUserID string
	}
	groups := make(map[key]*Group)
	conceptByGroup := make(map[key]string)

	for _, a := range agents {
		if a.SeedName == "" || a.OwnerUserID == "" {
			// Skip rows that aren't perUser seed rows. Caller's
			// SQL is meant to filter these out; the guard here
			// keeps ComputePlan honest if a caller passes a wider
			// row set.
			continue
		}
		k := key{seedName: a.SeedName, ownerUserID: a.OwnerUserID}
		g, ok := groups[k]
		if !ok {
			expectedID := canonicalIDFor(a.Concept, a.SeedName, a.OwnerUserID)
			g = &Group{
				SeedName:    a.SeedName,
				OwnerUserID: a.OwnerUserID,
				ExpectedID:  expectedID,
			}
			groups[k] = g
			conceptByGroup[k] = a.Concept
		}
		if a.ID == g.ExpectedID {
			g.Keep = append(g.Keep, a.ID)
		} else {
			g.Doomed = append(g.Doomed, a.ID)
		}
	}

	// Build the doomed-agent set so participant filtering is a
	// fast lookup rather than a nested scan.
	doomedAgents := make(map[string]struct{}, len(groups)*4)
	plan := Plan{}
	for _, g := range groups {
		if len(g.Keep) == 0 {
			g.NeedsReseed = true
		}
		plan.Groups = append(plan.Groups, *g)
		for _, id := range g.Doomed {
			doomedAgents[id] = struct{}{}
			plan.DoomedAgentIDs = append(plan.DoomedAgentIDs, id)
		}
	}

	for _, p := range participants {
		if _, ok := doomedAgents[p.AgentID]; ok {
			plan.DoomedParticipantIDs = append(plan.DoomedParticipantIDs, p.ID)
		}
	}

	return plan
}

// canonicalIDFor mirrors deterministicPerUserSeedId in
// component/memql/seed_materializer.go: bare userId (with the
// `v1:identity:user:` prefix stripped) concatenated to the seed
// name, then re-prefixed with the row's concept. Keeping the
// derivation in one helper here means a future change to the
// materializer's id format only needs to update both sites (the
// materializer's helper + this one) in lock-step.
func canonicalIDFor(concept, seedName, ownerUserID string) string {
	bare := strings.TrimPrefix(ownerUserID, "v1:identity:user:")
	return concept + ":" + seedName + "-" + bare
}
