package main

import (
	"sort"
	"testing"
)

func TestComputePlan_NoDupes(t *testing.T) {
	// One row, already at the canonical id -> nothing to do.
	agents := []AgentRow{
		{
			ID:          "v1:agents:agent:assistant-jose",
			Concept:     "v1:agents:agent",
			OwnerUserID: "v1:identity:user:jose",
			SeedName:    "assistant",
		},
	}
	plan := ComputePlan(agents, nil)

	if len(plan.DoomedAgentIDs) != 0 {
		t.Errorf("expected 0 doomed agents, got %v", plan.DoomedAgentIDs)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(plan.Groups))
	}
	if plan.Groups[0].NeedsReseed {
		t.Errorf("group with canonical keeper must not need reseed")
	}
	if len(plan.Groups[0].Keep) != 1 {
		t.Errorf("expected 1 keeper, got %v", plan.Groups[0].Keep)
	}
}

func TestComputePlan_AllDoomedNeedsReseed(t *testing.T) {
	// The DB state right after a pre-#274 cluster boot: every row
	// is a random UUID, none match the deterministic id. Every row
	// is doomed and the group needs a reseed.
	agents := []AgentRow{
		{ID: "v1:agents:agent:abc111", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:def222", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:ghi333", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
	}
	plan := ComputePlan(agents, nil)

	if len(plan.DoomedAgentIDs) != 3 {
		t.Errorf("expected 3 doomed agents, got %v", plan.DoomedAgentIDs)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(plan.Groups))
	}
	if !plan.Groups[0].NeedsReseed {
		t.Errorf("group with no keeper must need reseed")
	}
	if plan.Groups[0].ExpectedID != "v1:agents:agent:assistant-jose" {
		t.Errorf("ExpectedID = %q, want v1:agents:agent:assistant-jose", plan.Groups[0].ExpectedID)
	}
}

func TestComputePlan_KeeperPresentDoomsTheRest(t *testing.T) {
	// Mixed: the canonical row landed (e.g. after one cluster
	// restart post-#274) but the legacy UUIDs are still around.
	// The keeper survives; everything else is doomed; no reseed
	// needed.
	agents := []AgentRow{
		{ID: "v1:agents:agent:abc111", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:assistant-jose", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:def222", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
	}
	plan := ComputePlan(agents, nil)

	if len(plan.DoomedAgentIDs) != 2 {
		t.Errorf("expected 2 doomed agents, got %v", plan.DoomedAgentIDs)
	}
	if len(plan.Groups[0].Keep) != 1 || plan.Groups[0].Keep[0] != "v1:agents:agent:assistant-jose" {
		t.Errorf("Keep = %v, want exactly v1:agents:agent:assistant-jose", plan.Groups[0].Keep)
	}
	if plan.Groups[0].NeedsReseed {
		t.Errorf("group with canonical keeper must not need reseed")
	}
}

func TestComputePlan_MultipleSeedsPerOwner(t *testing.T) {
	// The live cluster shape: same user has dupes across multiple
	// perUser seeds (assistant, plannerAgent, trainerAgent).
	// Each (seed, owner) pair is its own group with its own
	// expected id.
	agents := []AgentRow{
		{ID: "v1:agents:agent:a1", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:a2", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:p1", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "plannerAgent"},
		{ID: "v1:agents:agent:p2", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "plannerAgent"},
		{ID: "v1:agents:agent:t1", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "trainerAgent"},
	}
	plan := ComputePlan(agents, nil)

	if len(plan.Groups) != 3 {
		t.Fatalf("expected 3 groups (one per seed name), got %d", len(plan.Groups))
	}
	// Every group needs reseed (no keeper landed for any of them).
	for _, g := range plan.Groups {
		if !g.NeedsReseed {
			t.Errorf("group %s/%s should need reseed", g.SeedName, g.OwnerUserID)
		}
	}
	if len(plan.DoomedAgentIDs) != 5 {
		t.Errorf("expected 5 total doomed agents, got %d", len(plan.DoomedAgentIDs))
	}
}

func TestComputePlan_MultipleOwnersIndependent(t *testing.T) {
	// Two users, each with their own dupe set. The groups must
	// stay independent -- jose's keeper should not satisfy
	// alice's group, and vice versa.
	agents := []AgentRow{
		{ID: "v1:agents:agent:assistant-jose", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:abc111", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:def222", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:alice", SeedName: "assistant"},
		{ID: "v1:agents:agent:ghi333", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:alice", SeedName: "assistant"},
	}
	plan := ComputePlan(agents, nil)

	// Jose's group has a keeper, Alice's doesn't.
	for _, g := range plan.Groups {
		switch g.OwnerUserID {
		case "v1:identity:user:jose":
			if g.NeedsReseed {
				t.Errorf("jose has a canonical keeper; should not need reseed")
			}
			if len(g.Doomed) != 1 {
				t.Errorf("jose should have 1 doomed row, got %v", g.Doomed)
			}
		case "v1:identity:user:alice":
			if !g.NeedsReseed {
				t.Errorf("alice has no canonical keeper; should need reseed")
			}
			if len(g.Doomed) != 2 {
				t.Errorf("alice should have 2 doomed rows, got %v", g.Doomed)
			}
		}
	}
}

func TestComputePlan_ParticipantsDoomedWithTheirAgents(t *testing.T) {
	// Three participants point at three of five doomed agents
	// (the live daily-space state from #273). All three
	// participants must be in DoomedParticipantIDs.
	agents := []AgentRow{
		{ID: "v1:agents:agent:a1", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:a2", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
		{ID: "v1:agents:agent:a3", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
	}
	participants := []ParticipantRow{
		{ID: "v1:cognition:participant:p1", AgentID: "v1:agents:agent:a1"},
		{ID: "v1:cognition:participant:p2", AgentID: "v1:agents:agent:a2"},
		{ID: "v1:cognition:participant:p3", AgentID: "v1:agents:agent:a3"},
		// Participant pointing at a NON-seed agent -- must not
		// be touched. The dedupe migration only touches rows it
		// can prove are duplicates of a perUser seed.
		{ID: "v1:cognition:participant:p4", AgentID: "v1:agents:agent:user-created-specialist"},
	}
	plan := ComputePlan(agents, participants)

	sort.Strings(plan.DoomedParticipantIDs)
	want := []string{
		"v1:cognition:participant:p1",
		"v1:cognition:participant:p2",
		"v1:cognition:participant:p3",
	}
	if len(plan.DoomedParticipantIDs) != len(want) {
		t.Fatalf("DoomedParticipantIDs = %v, want %v", plan.DoomedParticipantIDs, want)
	}
	for i, id := range want {
		if plan.DoomedParticipantIDs[i] != id {
			t.Errorf("DoomedParticipantIDs[%d] = %q, want %q", i, plan.DoomedParticipantIDs[i], id)
		}
	}
}

func TestComputePlan_SkipsRowsWithoutOwnerOrSeed(t *testing.T) {
	// Defensive: if the caller passes a wider row set than the
	// SQL is supposed to scope, the planner skips rows that
	// aren't perUser-seed rows. No spurious dooms.
	agents := []AgentRow{
		{ID: "v1:agents:agent:globalSeed", Concept: "v1:agents:agent", SeedName: "broadcaster"},                              // no owner
		{ID: "v1:agents:agent:userCreated", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: ""},  // not seeded
		{ID: "v1:agents:agent:assistant-jose", Concept: "v1:agents:agent", OwnerUserID: "v1:identity:user:jose", SeedName: "assistant"},
	}
	plan := ComputePlan(agents, nil)

	if len(plan.Groups) != 1 {
		t.Fatalf("expected 1 group (assistant-jose only), got %d", len(plan.Groups))
	}
	if len(plan.DoomedAgentIDs) != 0 {
		t.Errorf("expected 0 doomed (the keeper is canonical), got %v", plan.DoomedAgentIDs)
	}
}

func TestCanonicalIDFor_StripsUserPrefix(t *testing.T) {
	// Mirrors deterministicPerUserSeedId in seed_materializer.go.
	got := canonicalIDFor("v1:agents:agent", "assistant", "v1:identity:user:395e4e72-3097-4371-b9be-18da56eb8d5a")
	want := "v1:agents:agent:assistant-395e4e72-3097-4371-b9be-18da56eb8d5a"
	if got != want {
		t.Errorf("canonicalIDFor = %q, want %q", got, want)
	}
}

func TestCanonicalIDFor_NoPrefixToStrip(t *testing.T) {
	// Some event-payload code paths carry the bare userId.
	got := canonicalIDFor("v1:agents:agent", "assistant", "395e4e72-3097-4371-b9be-18da56eb8d5a")
	want := "v1:agents:agent:assistant-395e4e72-3097-4371-b9be-18da56eb8d5a"
	if got != want {
		t.Errorf("canonicalIDFor = %q, want %q", got, want)
	}
}
