//go:build agent

package worker

// THE SHARED MODEL CALL, ACROSS THE HOP (epic memql#5327, design D10).
//
// ===========================================================================
// THE PATH H-2 BROKE IS EXACTLY THE PATH NOTHING TESTED
// ===========================================================================
// The router picks a cluster-shared machine belonging to owner O for acting
// user U. The envelope crosses to the replica holding O's stream, and the
// receiver resolved it through `WorkersForOwner(U)` -- which by construction
// cannot return O's machine. So it refused `registration_refused`, and the
// call died as `no_local_model_available`: the refusal that means "your fleet
// is asleep", told to somebody looking at a lent machine they can see is on.
//
// It worked exactly when the stream happened to be LOCAL. On the default
// two-replica topology that is a coin toss, which is why the audit's coverage
// table has "shared user allowed ACROSS the hop" as the one row reading
// **No** -- and why the in-process plan test beside it stayed green the whole
// time.
//
// IN PROCESS, not clustere2e, for the reason every other hop test in this
// directory gives: a live-cluster gate is skipped on every CI lane and every
// developer machine, and a gate skipped by default cannot be what stands
// between a feature and the bug it prevents.
//
// TO CONFIRM THESE ARE LOAD-BEARING: point the receiver back at
// verifyRegistration instead of verifySharedRegistration, and the first three
// fail while the last two keep passing -- which is the shape that matters,
// because the widening must not have widened anything else.

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const sharedHopModel = "llama3.1:8b"

// sharedHop is one machine owned by OWNER, held by replica B, with a turn
// running for CALLER on replica A.
type sharedHop struct {
	link   *meshLink
	owner  string
	caller string
}

const (
	sharedHopOwner  = "v1:identity:user:olivia"
	sharedHopCaller = "v1:identity:user:ursula"
)

// newSharedHop stands up both replicas. `shareMode` is the OWNER's half of the
// consent and `serve` the machine's half, so a test can withhold either one
// and see which refusal it gets.
func newSharedHop(t *testing.T, shareMode string, cockpitServes bool, withStore bool) *sharedHop {
	t.Helper()

	regB := workerservice.NewRegistry(testLogger(), fleetNow)
	w := &workerservice.Worker{
		RegistrationId: "studio",
		OwnerUserId:    sharedHopOwner,
		Name:           "studio",
		Capabilities:   []string{workerservice.CapabilityHeadless, workerservice.ModelCapability},
		Labels:         map[string]string{workerservice.ModelLabel(sharedHopModel): "1"},
		Concurrency:    map[string]uint32{workerservice.ModelCapability: 2},
	}
	w.SetModelCallFunc(func(ctx context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		h, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		go finish(workerservice.ModelCallOutcome{
			FinishReason: workerservice.ModelFinishStop,
			Content:      "served",
		})
		return h, nil
	})
	regB.Add(w)

	cand := machine("studio")
	cand.OwnerUserId = sharedHopOwner
	cand.ConnectedNodeId = nodeB
	cand.Capabilities = []string{workerservice.CapabilityHeadless, workerservice.ModelCapability}
	cand.Labels = map[string]string{workerservice.ModelLabel(sharedHopModel): "1"}
	cand.SharingMode = shareMode
	cand.InferenceServe = ""
	if cockpitServes {
		cand.InferenceServe = "cluster"
	}

	// THE OWNED READ ANSWERS NOTHING FOR THE CALLER, which is the whole
	// premise: `owner` scopes fakeFleet exactly as workersForOwner's filter
	// does, so a receiver asking "is this Ursula's machine" gets no rows.
	fleet := &fakeFleet{machines: []Candidate{cand}, owner: sharedHopOwner}
	var store FleetStore = &sharedFleet{fakeFleet: fleet, all: []Candidate{cand}}
	if !withStore {
		// A replica with no cross-owner read at all. It must degrade to
		// owner-only rather than to admitting everything.
		store = fleet
	}

	link := &meshLink{t: t, reachable: true}
	link.handler = NewForwardHandler(regB, store, testLogger())
	link.router = newForwardRouter(link, func() (string, string) { return nodeA, "agent" }, testLogger())
	return &sharedHop{link: link, owner: sharedHopOwner, caller: sharedHopCaller}
}

func (h *sharedHop) call(t *testing.T, actingUserId string) (ModelForwardOutcome, error) {
	t.Helper()
	start := &memqlv1.ModelCallStart{
		RequestId: "outer",
		Model:     sharedHopModel,
		Kind:      workerservice.ModelCallKindChat,
		Messages:  []*memqlv1.ModelCallMessage{{Role: "user", Content: "hello"}},
		Limits:    &memqlv1.ModelCallLimits{TimeoutSeconds: 10, IdleTimeoutSeconds: 5, KeepaliveSeconds: 1},
	}
	return h.link.router.ForwardModelCall(
		authorityCtx(t, actingUserId), nodeB, "studio", actingUserId, start, 10*time.Second, nil)
}

func TestASharedMachineServesAForeignCallerAcrossTheHop(t *testing.T) {
	// THE WHOLE OF H-2. Both consents given, the machine is on replica B, the
	// turn is on replica A, and the caller does not own it.
	h := newSharedHop(t, "cluster", true, true)

	out, err := h.call(t, h.caller)
	if err != nil {
		t.Fatalf("ForwardModelCall: %v -- a cluster-shared machine must serve a foreign caller "+
			"across the hop; before design D10 this refused registration_refused and surfaced as "+
			"no_local_model_available, told to somebody looking at a lent machine they can see is on", err)
	}
	if out.RefusedBeforeStart {
		t.Fatalf("refused before start: %s %s", out.ErrorCode, out.ErrorMessage)
	}
	if out.End.GetContent() != "served" {
		t.Fatalf("the call did not reach the machine: %q", out.End.GetContent())
	}
}

func TestAPrivateMachineStillRefusesAForeignCallerAcrossTheHop(t *testing.T) {
	// THE NEGATIVE CONTROL FOR THE TEST ABOVE. Widening the receiver to admit
	// shared machines is only worth having if it did not admit everything --
	// and a widening that did would pass the first test and nothing else.
	h := newSharedHop(t, "owner", true, true)

	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("a machine its owner has NOT shared must not serve a foreign caller")
	}
}

func TestTheOwnersOwnCallStillReachesTheirMachineAcrossTheHop(t *testing.T) {
	// THE POSITIVE CONTROL. The owned arm runs first and unconditionally, so
	// a machine the caller owns is admitted without the cross-owner read
	// happening at all -- including one they have never shared.
	h := newSharedHop(t, "owner", false, true)

	out, err := h.call(t, h.owner)
	if err != nil {
		t.Fatalf("an owner's own machine must still be reachable: %v", err)
	}
	if out.RefusedBeforeStart {
		t.Fatalf("refused before start: %s %s", out.ErrorCode, out.ErrorMessage)
	}
}

func TestAReplicaWithNoCrossOwnerReadDegradesToOwnerOnly(t *testing.T) {
	// THE HONEST DEGRADATION. A node that cannot make the cross-owner read
	// cannot establish that anything was shared, so it answers exactly as it
	// did before D10: only owned machines. Admitting on a weaker check than
	// the one this exists to perform is the alternative, and it is the wrong
	// one.
	h := newSharedHop(t, "cluster", true, false)

	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("a replica with no SharedFleetStore must not admit a foreign caller")
	}
}

func TestBothConsentsAreStillNeededAcrossTheHop(t *testing.T) {
	// THE MACHINE'S OWN HALF. The owner has offered it and the cockpit has
	// not, which is the state a person is most likely to be in: they pressed
	// the button on the Fleet page and have not touched policy.yaml. The
	// receiver must not treat the owner's half as the whole consent.
	h := newSharedHop(t, "cluster", false, true)

	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("both halves of the consent are needed, on the receiver as well as in the router")
	}
}

// ---------------------------------------------------------------------------
// Lent to NAMED people, across the hop (epic memql#5344)
// ---------------------------------------------------------------------------
//
// The receiver re-decides on ITS OWN reads, as it does for a cluster share:
// the list comes off the row it reads, and a group member's groups come from
// the membership source ON THE RECEIVING REPLICA -- never from anything the
// envelope carries, which is a claim the sender made rather than a fact.

// newPeopleHop is newSharedHop with the owner's half set to a people share and
// the receiver's group reads answered by groupsOf.
func newPeopleHop(t *testing.T, userIds, groupIds []string, groupsOf workerservice.GroupResolver) *sharedHop {
	t.Helper()
	h := newSharedHop(t, workerservice.SharingModePeople, true, true)
	store, ok := h.link.handler.store.(*sharedFleet)
	if !ok {
		t.Fatal("the hop's receiver has no cross-owner read")
	}
	for i := range store.all {
		store.all[i].SharedUserIds = userIds
		store.all[i].SharedGroupIds = groupIds
	}
	h.link.handler.SetGroupResolver(groupsOf)
	return h
}

func TestAListedPersonIsServedAcrossTheHop(t *testing.T) {
	h := newPeopleHop(t, []string{"ursula"}, nil, nil) // stored bare, asked canonical
	out, err := h.call(t, h.caller)
	if err != nil || out.RefusedBeforeStart {
		t.Fatalf("a listed person must be served across the hop: %v %s %s", err, out.ErrorCode, out.ErrorMessage)
	}
	if out.End.GetContent() != "served" {
		t.Fatalf("the call did not reach the machine: %q", out.End.GetContent())
	}
}

func TestAGroupMemberIsServedAcrossTheHopByTheReceiversOwnRead(t *testing.T) {
	asked := ""
	groupsOf := func(_ context.Context, userId string) []string {
		asked = userId
		if sameSubject(userId, sharedHopCaller) {
			return []string{"v1:identity:group:design"}
		}
		return nil
	}
	h := newPeopleHop(t, nil, []string{"design"}, groupsOf)
	out, err := h.call(t, h.caller)
	if err != nil || out.RefusedBeforeStart {
		t.Fatalf("a member of a listed group must be served across the hop: %v %s %s", err, out.ErrorCode, out.ErrorMessage)
	}
	if !sameSubject(asked, sharedHopCaller) {
		t.Fatalf("the receiver resolved groups for %q; it must ask about the VERIFIED authority's subject", asked)
	}
}

func TestSomebodyNotOnTheListIsRefusedAcrossTheHop(t *testing.T) {
	// The negative control for the two above: a widening that admitted
	// everyone would pass both of them and fail only this.
	h := newPeopleHop(t, []string{"v1:identity:user:somebody-else"}, []string{"design"},
		func(context.Context, string) []string { return nil })
	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("a person the owner did not list must not be served across the hop")
	}
}

func TestAListedPersonStillNeedsTheMachineToAgreeAcrossTheHop(t *testing.T) {
	h := newPeopleHop(t, []string{sharedHopCaller}, nil, nil)
	store := h.link.handler.store.(*sharedFleet)
	for i := range store.all {
		store.all[i].InferenceServe = ""
	}
	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("the cockpit's half is still required for a people share, on the receiver too")
	}
}

// ---------------------------------------------------------------------------
// D15 -- whose hardware served the call
// ---------------------------------------------------------------------------

func TestASharedCallRecordsWhoseMachineServedIt(t *testing.T) {
	// M-10. v1:router:call.machineOwnerUserId was documented as "Empty until
	// shared team machines land". They landed and the field stayed empty, so
	// no row in any cluster said that one person's call had run on another
	// person's hardware -- the single fact a shared fleet adds to the ledger,
	// and the one the person who lent the machine is entitled to.
	cand := machine("studio")
	cand.OwnerUserId = sharedHopOwner

	if got := machineOwnerAttribution(cand, sharedHopCaller); got != sharedHopOwner {
		t.Fatalf("a shared call must record the machine's owner, got %q", got)
	}
}

func TestACallOnYourOwnMachineRecordsNoOwner(t *testing.T) {
	// EMPTY ON YOUR OWN MACHINE, deliberately. The field answers "whose
	// machine, if not yours"; repeating userId in the self case would turn
	// "was this shared" from a value a reader can read into a comparison every
	// reader has to make.
	cand := machine("studio")
	cand.OwnerUserId = sharedHopOwner

	if got := machineOwnerAttribution(cand, sharedHopOwner); got != "" {
		t.Fatalf("a call on your own machine must record no owner, got %q", got)
	}
}

func TestOwnerAttributionToleratesTheBareCanonicalSplit(t *testing.T) {
	// The engine bare-ifies ids on egress, so a row's ownerUserId and an
	// acting subject are routinely the same person in two spellings. Comparing
	// them raw would attribute every one of somebody's own calls to
	// themselves as though it were shared.
	cand := machine("studio")
	cand.OwnerUserId = "v1:identity:user:olivia"

	if got := machineOwnerAttribution(cand, "olivia"); got != "" {
		t.Fatalf("the bare and canonical forms of one subject must compare equal, got %q", got)
	}
}
