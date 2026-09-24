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

// ---------------------------------------------------------------------------
// Lent to NAMED people (epic memql#5344, design G3 and G7)
// ---------------------------------------------------------------------------

func peopleMachine(id, owner string, userIds, groupIds []string) Candidate {
	c := sharedMachine(id, owner)
	c.SharingMode = workerservice.SharingModePeople
	c.SharedUserIds = userIds
	c.SharedGroupIds = groupIds
	return c
}

// equalIds treats nil and empty as the same answer: "no candidates".
func equalIds(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestAPeopleShareServesExactlyItsPeople(t *testing.T) {
	// G7 on the plan: the listed person, a member of the listed group, and
	// nobody else -- in whichever spelling the list and the caller use.
	forAna := peopleMachine("for-ana", "bob", []string{"v1:identity:user:ana"}, nil)
	forDesign := peopleMachine("for-design", "bob", nil, []string{"design"})
	store := &sharedFleet{fakeFleet: &fakeFleet{owner: "ana"}, all: []Candidate{forAna, forDesign}}
	router := modelRouter(t, store)
	router.SetGroupResolver(func(_ context.Context, userId string) []string {
		if userId == "cy" {
			return []string{"v1:identity:group:design"}
		}
		return nil
	})
	cases := map[string][]string{"ana": {"for-ana"}, "cy": {"for-design"}, "dee": nil}
	for user, want := range cases {
		plan, err := router.PlanUserModelWithShared(context.Background(), user, smallModel, ModelNeeds{})
		if err != nil {
			t.Fatalf("%s: %v", user, err)
		}
		if got := ids(plan.Candidates); !equalIds(got, want) {
			t.Fatalf("%s: candidates = %v, want %v", user, got, want)
		}
	}
}

func TestAPeopleShareRefusalIsForeignNoiseForEverybodyElse(t *testing.T) {
	// dee is on no list. With no machine of her own, the refusal for bob's
	// machine is the only signal she has -- and it is the COUNTED kind (D12),
	// never a line naming bob's machine.
	forAna := peopleMachine("for-ana", "bob", []string{"ana"}, nil)
	store := &sharedFleet{fakeFleet: &fakeFleet{owner: "dee"}, all: []Candidate{forAna}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "dee", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	why, ok := plan.Rejected["for-ana"]
	if !ok || !strings.Contains(why, "specific people") {
		t.Fatalf("rejected = %v; the refusal must say the machine is lent to specific people", plan.Rejected)
	}
}

func TestSystemWorkNeverReachesAPeopleShare(t *testing.T) {
	// G3. A list of names is consent for those people; the cluster's own work
	// is nobody on the list, so it reaches only a machine lent to everyone.
	forAna := peopleMachine("for-ana", "bob", []string{"ana"}, nil)
	everyone := sharedMachine("everyone", "bob")
	store := &sharedFleet{fakeFleet: &fakeFleet{}, all: []Candidate{forAna, everyone}}
	plan, err := modelRouter(t, store).PlanSharedModel(context.Background(), smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "everyone" {
		t.Fatalf("system candidates = %v, want only the machine lent to everyone", got)
	}
	if why := plan.Rejected["for-ana"]; !strings.Contains(why, "cluster's own work") {
		t.Fatalf("the refusal must say why: %q", why)
	}
	// And through the user entry point with no acting user, which is how an
	// automation arrives there.
	viaUser, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(viaUser.Candidates); len(got) != 1 || got[0] != "everyone" {
		t.Fatalf("no-actor candidates = %v, want only the machine lent to everyone", got)
	}
}

func TestOwnMachinesStillComeFirstBeforeAPeopleShare(t *testing.T) {
	// preferOwnMachines is unchanged by who the other machine is lent to.
	mine := privateMachine("mine", "ana")
	forAna := peopleMachine("for-ana", "bob", []string{"ana"}, nil)
	store := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "ana"}, all: []Candidate{mine, forAna}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "ana", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 2 || got[0] != "mine" || got[1] != "for-ana" {
		t.Fatalf("preferOwnMachines: %v", got)
	}
}

func TestAGroupShareAdmitsNobodyWhenMembershipsCannotBeRead(t *testing.T) {
	// The narrowing direction: an unreadable membership costs the group arm,
	// never widens it.
	forDesign := peopleMachine("for-design", "bob", nil, []string{"design"})
	store := &sharedFleet{fakeFleet: &fakeFleet{owner: "cy"}, all: []Candidate{forDesign}}
	router := modelRouter(t, store)
	router.SetGroupResolver(func(context.Context, string) []string { return nil })
	plan, err := router.PlanUserModelWithShared(context.Background(), "cy", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 0 {
		t.Fatalf("candidates = %v; nobody is admitted through a group nobody could read", got)
	}
}
