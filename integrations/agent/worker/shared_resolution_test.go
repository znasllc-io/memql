//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

func sharedMachine(id, owner string) Candidate {
	c := modelMachine(id, map[string]ModelAttributes{smallModel: {}})
	c.OwnerUserId = owner
	c.SharingMode = workerservice.SharingModeCluster
	c.InferenceServe = workerservice.InferenceServeCluster
	return c
}

func privateMachine(id, owner string) Candidate {
	c := modelMachine(id, map[string]ModelAttributes{smallModel: {}})
	c.OwnerUserId = owner
	return c
}

func TestAnotherUsersSharedMachineIsAvailableToMe(t *testing.T) {
	// The whole point of D6. Before it, a user's call could land only on
	// hardware they owned, so a business with one Mac Studio in the office had
	// no way to make it serve the team.
	mine := privateMachine("mine", "alice")
	theirs := sharedMachine("theirs", "bob")

	store := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"}, all: []Candidate{mine, theirs}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	got := ids(plan.Candidates)
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want my machine and the shared one", got)
	}
	if got[0] != "mine" {
		t.Fatalf("preferOwnMachines: my own machine must come first, got %v", got)
	}
	if got[1] != "theirs" {
		t.Fatalf("the shared machine must be reachable, got %v", got)
	}
}

func TestAnotherUsersUNSHAREDMachineIsNotAvailableToMe(t *testing.T) {
	// The boundary that does NOT move. A machine reaches the shared half only
	// because its owner put it there.
	mine := privateMachine("mine", "alice")
	theirs := privateMachine("theirs", "bob")

	store := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"}, all: []Candidate{mine, theirs}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("candidates = %v, want only my own machine", got)
	}
	// Owner Ask with a live owned worker must not surface foreign SharingRefusal
	// (prod: 24ad under jmendivil). The machine is simply absent from candidates.
	if why, ok := plan.Rejected["theirs"]; ok {
		t.Fatalf("foreign private share noise must be omitted when an own candidate exists, got %q", why)
	}
}

func TestEitherConsentMissingKeepsAMachineOutOfMyPlan(t *testing.T) {
	// Either half alone is not consent, on the USER path as well as the system
	// one. A person may own a machine they are not entitled to volunteer, and a
	// machine may sit somewhere its policy forbids serving strangers.
	mine := privateMachine("mine", "alice")

	ownerOnly := privateMachine("owner-only", "bob")
	ownerOnly.SharingMode = workerservice.SharingModeCluster

	cockpitOnly := privateMachine("cockpit-only", "carol")
	cockpitOnly.InferenceServe = workerservice.InferenceServeCluster

	store := &sharedFleet{
		fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"},
		all:       []Candidate{mine, ownerOnly, cockpitOnly},
	}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("candidates = %v, want only my own machine", got)
	}
	for _, id := range []string{"owner-only", "cockpit-only"} {
		if why, ok := plan.Rejected[id]; ok {
			t.Fatalf("%s share noise must be omitted when an own candidate exists, got %q", id, why)
		}
	}
}

func TestRevokingSharingTakesEffectOnTheNextResolution(t *testing.T) {
	// Revoking is a change to a row, and resolution reads the row. There is no
	// cache to invalidate and no in-flight call to interrupt: the machine is in
	// the plan or it is not, and the next call asks again.
	mine := privateMachine("mine", "alice")
	theirs := sharedMachine("theirs", "bob")
	store := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"}, all: []Candidate{mine, theirs}}
	router := modelRouter(t, store)

	before, err := router.PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Candidates) != 2 {
		t.Fatalf("before revoke: %v", ids(before.Candidates))
	}

	// The owner revokes.
	store.all[1].SharingMode = workerservice.SharingModeOwner

	after, err := router.PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(after.Candidates); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("after revoke: %v, want only my own machine", got)
	}
}

func TestMyOwnMachineNeverAppearsTwice(t *testing.T) {
	// The cross-owner read returns EVERY machine in the cluster, mine included.
	// Without the skip my own machine would be in both halves and be tried
	// twice, which on a refusal-before-start would burn a re-pick on the same
	// hardware.
	mine := sharedMachine("mine", "alice") // shared AND mine
	store := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"}, all: []Candidate{mine}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 1 {
		t.Fatalf("candidates = %v, want my machine exactly once", got)
	}
}

func TestSystemWorkFallsThroughToTheSharedPlan(t *testing.T) {
	// No acting user is system work, which has no own half at all. Falling
	// through is the honest answer rather than an empty one.
	theirs := sharedMachine("theirs", "bob")
	store := &sharedFleet{fakeFleet: &fakeFleet{}, all: []Candidate{theirs}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "theirs" {
		t.Fatalf("candidates = %v", got)
	}
}

func TestOwnMachineFirstToleratesTheBareCanonicalSplit(t *testing.T) {
	// The row carries a canonical id and a token's subject may be bare. A naive
	// == would put every one of a person's own machines in the SHARED half,
	// which would still work and would quietly stop preferring their hardware.
	c := Candidate{OwnerUserId: "v1:identity:user:alice"}
	if !OwnMachineFirst(c, "alice") {
		t.Fatal("a bare subject must match the canonical owner id")
	}
	if !OwnMachineFirst(c, "v1:identity:user:alice") {
		t.Fatal("a canonical subject must match itself")
	}
	if OwnMachineFirst(c, "bob") {
		t.Fatal("a different user must not match")
	}
	if OwnMachineFirst(Candidate{}, "alice") {
		t.Fatal("an unowned machine must not match anybody")
	}
}

func TestOwnerPrivateMachineWorksWithoutShareWhenOwnReadMisses(t *testing.T) {
	// Product lock: pairing unlocks Ask/Materialize/Nexus for THAT user without
	// Fleet share + inference.serve:cluster. Those consents are only for a
	// different account.
	//
	// Regression: WorkersForOwner empty while the cross-owner read still listed
	// the owner's private machine. PlanUserModelWithShared used to continue on
	// sameSubjectId and drop it, then surface SharingRefusal for other private
	// machines -- the owner Ask "Neither consent… Both are needed" symptom.
	mine := privateMachine("mine", "v1:identity:user:alice")
	store := &sharedFleet{
		fakeFleet: &fakeFleet{machines: nil, owner: "alice"},
		all:       []Candidate{mine},
	}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("candidates = %v, want the owner's private machine without share", got)
	}
	for id, why := range plan.Rejected {
		if strings.Contains(why, "consent") || strings.Contains(why, "inference.serve") {
			t.Fatalf("owner machine must not be refused for cluster share: %s: %s", id, why)
		}
	}
}

func TestOwnerMachineAlreadyRuledOutIsNotReadmittedViaSharedList(t *testing.T) {
	mine := privateMachine("mine", "alice")
	mine.ConnectedNodeId = ""
	mine.LastSeenAt = fleetNow().Add(-24 * time.Hour)
	store := &sharedFleet{
		fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"},
		all:       []Candidate{mine},
	}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 0 {
		t.Fatalf("candidates = %v, want none -- own plan already ruled it offline", got)
	}
	if why := plan.Rejected["mine"]; why != "offline" {
		t.Fatalf("rejected[mine] = %q, want offline from the own plan", why)
	}
}

func TestOwnerPrivateMachineWithEmptyOwnerUserIdIsNotShareRefusal(t *testing.T) {
	// Residual #5270: if the shared row lacks OwnerUserId, recovery cannot
	// match ownership -- but it must not dress that as "Neither consent".
	orphan := privateMachine("orphan", "")
	store := &sharedFleet{
		fakeFleet: &fakeFleet{machines: nil, owner: "alice"},
		all:       []Candidate{orphan},
	}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	why := plan.Rejected["orphan"]
	if why != "registration missing ownerUserId" {
		t.Fatalf("rejected[orphan] = %q, want missing ownerUserId (not a share refusal)", why)
	}
	if strings.Contains(why, "consent") || strings.Contains(why, "inference.serve") {
		t.Fatalf("must not look like a cluster-share refusal: %q", why)
	}
}

func TestOwnerActiveMachinePreferredOverForeignPrivateShareNoise(t *testing.T) {
	// Prod shape: passkey user owns an active machine; shared list also has
	// another account's private (or revoked) registration. Owner Ask must
	// admit the active own machine without cluster-share.
	mine := privateMachine("mine-active", "v1:identity:user:alice")
	theirs := privateMachine("theirs", "v1:identity:user:bob")
	store := &sharedFleet{
		fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "alice"},
		all:       []Candidate{mine, theirs},
	}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "mine-active" {
		t.Fatalf("candidates = %v, want only the owner's active machine", got)
	}
}

func TestForeignShareRefusalSurfacesWhenNoOwnCandidate(t *testing.T) {
	// When the owner has no live machine of their own, foreign private share
	// refusals remain the actionable signal (system / empty own fleet).
	foreign := privateMachine("24ad", "jmendivil")
	store := &sharedFleet{fakeFleet: &fakeFleet{machines: nil, owner: "jose"}, all: []Candidate{foreign}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "jose", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanUserModelWithShared: %v", err)
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %v, want none", ids(plan.Candidates))
	}
	if why := plan.Rejected["24ad"]; !strings.Contains(why, "owner has not shared") && !strings.Contains(why, "Both are needed") && !strings.Contains(strings.ToLower(why), "share") {
		t.Fatalf("sole foreign private refusal must stay visible, got %q", why)
	}
}
