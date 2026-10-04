//go:build agent

package worker

// A PIN TO A MACHINE LENT TO YOU (memql#5662, decided 2026-10-04).
//
// A pinned call names the one machine that must run it -- "Ask it something"
// on a machine's page is one. Pins resolved through PlanModel, which reads the
// caller's OWN machines only, so a pin to a machine somebody lent them was
// refused with the pin's own sentence while the same call unpinned could land
// on that very machine. The owner's ruling: a pin reaches every machine
// ServesPerson admits, and nothing else.
//
// Each case runs the whole path in process: the sender's plan on replica A,
// the forward, and the receiver's own re-check on replica B, which holds the
// lent machine's stream. The caller also owns a machine on replica A that
// offers the same model, so a pin that fell through to another candidate would
// be seen landing there.
//
// TO CONFIRM THESE ARE LOAD-BEARING: point FleetInference.Call's pin branch
// back at PlanModel and the three "lent" cases fail with the pin's refusal,
// while every refusal case keeps passing -- the widening must not widen
// anything else.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

const (
	lentPin      = "v1:worker:registration:studio" // the spelling the AiChat seam binds
	ownMachineId = "own-laptop"
	pinRefusal   = "selected machine"
	lentAnswer   = "served by the lent machine"
	ownAnswer    = "served by the caller's own machine"
)

// lentPinHop is one turn for sharedHopCaller on replica A, a machine owned by
// sharedHopOwner held by replica B, and the caller's own machine on replica A.
type lentPinHop struct {
	f           *FleetInference
	link        *meshLink
	receiverAll *sharedFleet // replica B's cross-owner read, for a case that changes it after planning
	studioCalls *atomic.Int32
	ownCalls    *atomic.Int32
}

// newLentPinHop builds both replicas. `lend` sets the OWNER's and the
// machine's halves of the consent on the lent machine's row; both replicas
// read that same row. groupsOf answers who is in which group, on both
// replicas.
func newLentPinHop(t *testing.T, lend func(*Candidate), groupsOf workerservice.GroupResolver) *lentPinHop {
	t.Helper()
	studioCalls, ownCalls := &atomic.Int32{}, &atomic.Int32{}
	serving := func(calls *atomic.Int32, content string) workerservice.ModelCallFunc {
		return func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
			calls.Add(1)
			h, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
			go finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Content: content})
			return h, nil
		}
	}
	modelCaps := []string{workerservice.CapabilityHeadless, workerservice.ModelCapability}
	// A window the provider path's working-context floor clears.
	modelLabels := map[string]string{workerservice.ModelLabel(sharedHopModel): "ctx=32768"}

	// Replica B holds the lent machine's stream.
	regB := workerservice.NewRegistry(testLogger(), fleetNow)
	studioWorker := &workerservice.Worker{
		RegistrationId: "studio", OwnerUserId: sharedHopOwner, Name: "studio",
		Capabilities: modelCaps, Labels: modelLabels,
		Concurrency: map[string]uint32{workerservice.ModelCapability: 2},
	}
	studioWorker.SetModelCallFunc(serving(studioCalls, lentAnswer))
	regB.Add(studioWorker)

	studio := machine("studio")
	studio.OwnerUserId = sharedHopOwner
	studio.ConnectedNodeId = nodeB
	studio.Capabilities = modelCaps
	studio.Labels = modelLabels
	studio.SharingMode = workerservice.SharingModeOwner // private until the case lends it
	if lend != nil {
		lend(&studio)
	}

	// The caller's own machine, on replica A, offering the same model.
	own := machine(ownMachineId)
	own.OwnerUserId = sharedHopCaller
	own.ConnectedNodeId = nodeA
	own.Capabilities = modelCaps
	own.Labels = modelLabels

	// Replica B reads the lent machine's row for its owner and across owners.
	receiverAll := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{studio}, owner: sharedHopOwner}, all: []Candidate{studio}}
	link := &meshLink{t: t, reachable: true}
	link.handler = NewForwardHandler(regB, receiverAll, testLogger())
	link.handler.SetGroupResolver(groupsOf)
	link.router = newForwardRouter(link, func() (string, string) { return nodeA, "agent" }, testLogger())

	// Replica A plans over the caller's own machine and the cross-owner read.
	senderStore := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{own}, owner: sharedHopCaller}, all: []Candidate{own, studio}}
	f := newFleetInference(t, senderStore)
	f.selfNodeId, f.forward = nodeA, link.router
	f.router.SetGroupResolver(groupsOf)
	ownWorker := &workerservice.Worker{
		RegistrationId: ownMachineId, OwnerUserId: sharedHopCaller, Name: ownMachineId,
		Capabilities: modelCaps, Labels: modelLabels,
		Concurrency: map[string]uint32{workerservice.ModelCapability: 2},
	}
	ownWorker.SetModelCallFunc(serving(ownCalls, ownAnswer))
	f.registry.Add(ownWorker)

	return &lentPinHop{f: f, link: link, receiverAll: receiverAll, studioCalls: studioCalls, ownCalls: ownCalls}
}

func (h *lentPinHop) call(t *testing.T, actingUserId, pin string) (memqlengine.FleetCallResult, error) {
	t.Helper()
	// System work carries no person, so there is no authority to assert for
	// it: the call is planned (and refused) before any hop.
	ctx := context.Background()
	if actingUserId != "" {
		ctx = authorityCtx(t, actingUserId)
	}
	return h.f.Call(ctx, memqlengine.FleetCallRequest{
		ActingUserId:   actingUserId,
		RegistrationId: pin,
		ModelId:        sharedHopModel,
		Kind:           memqlengine.FleetKindChat,
	})
}

func lendTo(userIds, groupIds []string, machineAgrees bool) func(*Candidate) {
	return func(c *Candidate) {
		c.SharingMode = workerservice.SharingModePeople
		c.SharedUserIds = userIds
		c.SharedGroupIds = groupIds
		c.InferenceServe = ""
		if machineAgrees {
			c.InferenceServe = workerservice.InferenceServeCluster
		}
	}
}

func lendToEveryone(c *Candidate) {
	c.SharingMode = workerservice.SharingModeCluster
	c.InferenceServe = workerservice.InferenceServeCluster
}

func callerInDesign(_ context.Context, userId string) []string {
	if sameSubject(userId, sharedHopCaller) {
		return []string{"v1:identity:group:design"}
	}
	return nil
}

func TestAPinToAMachineLentToTheCallerIsServedAcrossTheHop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lend     func(*Candidate)
		groupsOf workerservice.GroupResolver
	}{
		// Stored bare, asked canonical: the comparison is the share list's own.
		{"lent to the caller by name", lendTo([]string{"ursula"}, nil, true), nil},
		{"lent to a group the caller is in", lendTo(nil, []string{"design"}, true), callerInDesign},
		{"lent to everyone", lendToEveryone, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLentPinHop(t, tc.lend, tc.groupsOf)
			res, err := h.call(t, sharedHopCaller, lentPin)
			if err != nil {
				t.Fatalf("a pin to a machine lent to the caller must be served (memql#5662): %v", err)
			}
			if res.Content != lentAnswer || res.ExecutionSurface != FleetSurfacePrefix+"studio" {
				t.Fatalf("the pinned call answered elsewhere: %+v", res)
			}
			if res.MachineOwnerUserId != sharedHopOwner {
				t.Fatalf("whose hardware served the call = %q, want the lender %q (D15)", res.MachineOwnerUserId, sharedHopOwner)
			}
			if h.studioCalls.Load() != 1 || h.ownCalls.Load() != 0 {
				t.Fatalf("lent=%d own=%d: the pin must reach exactly the machine it names", h.studioCalls.Load(), h.ownCalls.Load())
			}
		})
	}
}

func TestAPinToAMachineNotLentToTheCallerKeepsThePinsRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lend     func(*Candidate)
		groupsOf workerservice.GroupResolver
	}{
		{"kept private by its owner", nil, nil},
		{"lent to somebody else", lendTo([]string{"v1:identity:user:somebody-else"}, nil, true), nil},
		{"lent to a group the caller is not in", lendTo(nil, []string{"design"}, true), func(context.Context, string) []string { return nil }},
		{"lent to the caller, but the machine has not agreed", lendTo([]string{"ursula"}, nil, false), nil},
		{"lent to the caller, and revoked", func(c *Candidate) {
			lendTo([]string{"ursula"}, nil, true)(c)
			c.RevokedAt = fleetNow()
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLentPinHop(t, tc.lend, tc.groupsOf)
			_, err := h.call(t, sharedHopCaller, lentPin)
			assertPinRefusal(t, err)
			if h.studioCalls.Load() != 0 || h.ownCalls.Load() != 0 {
				t.Fatalf("lent=%d own=%d: a refused pin must dispatch nothing, and never fall through to another machine",
					h.studioCalls.Load(), h.ownCalls.Load())
			}
		})
	}
}

// assertPinRefusal holds a refused pin to the pin's own sentence: one entry,
// keyed by the pin rather than by a machine id, so the refusal reads the same
// whether the machine is offline, somebody else's, or does not exist -- and
// names neither the machine nor its owner.
func assertPinRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *memqlengine.FleetUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("want the pin's typed refusal, got %v", err)
	}
	if len(refusal.Considered) != 1 || refusal.Considered[pinRefusal] == "" {
		t.Fatalf("considered = %v, want exactly the pin's own entry", refusal.Considered)
	}
	for _, leak := range []string{"studio", "olivia"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the pin's refusal names %q: %v", leak, err)
		}
	}
}

func TestAPinToTheCallersOwnMachineIsUnchanged(t *testing.T) {
	// The positive control. The lent machine is lent to the caller and could
	// serve, so a pin to their OWN machine landing there would be the widening
	// going wrong in the other direction.
	h := newLentPinHop(t, lendTo([]string{"ursula"}, nil, true), nil)
	res, err := h.call(t, sharedHopCaller, ownMachineId)
	if err != nil {
		t.Fatalf("a pin to the caller's own machine: %v", err)
	}
	if res.Content != ownAnswer || res.MachineOwnerUserId != "" || h.ownCalls.Load() != 1 || h.studioCalls.Load() != 0 {
		t.Fatalf("own pin answered elsewhere: %+v own=%d lent=%d", res, h.ownCalls.Load(), h.studioCalls.Load())
	}
}

func TestTheReceiverStillReDecidesAPinnedCall(t *testing.T) {
	// The sender read the machine as lent to the caller; by the time the call
	// lands, the owner has taken it back. The receiver re-reads the row and
	// refuses before start -- and the pinned plan has nothing to fall through
	// to, so the caller's own machine is not used instead.
	h := newLentPinHop(t, lendTo([]string{"ursula"}, nil, true), nil)
	for i := range h.receiverAll.all {
		lendTo([]string{"v1:identity:user:somebody-else"}, nil, true)(&h.receiverAll.all[i])
	}
	_, err := h.call(t, sharedHopCaller, lentPin)
	if !errors.Is(err, memqlengine.ErrFleetUnavailable) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "not lent to you") {
		t.Fatalf("the receiver's own refusal must be the one reported: %v", err)
	}
	if h.studioCalls.Load() != 0 || h.ownCalls.Load() != 0 {
		t.Fatalf("lent=%d own=%d: a pin the receiver refused must not run anywhere", h.studioCalls.Load(), h.ownCalls.Load())
	}
}

func TestSystemWorkCannotPinAMachine(t *testing.T) {
	// A pin is a person's choice of machine. A call with no acting person, or
	// with any identity the cluster synthesized -- an automation running as
	// itself, a connector, the operator credential's stream -- is the
	// cluster's own work and picks nothing: even a machine lent to everyone,
	// which unpinned system work may use. auth.NamesNoPerson is the rule, the
	// one the app gate applies.
	for _, tc := range []struct {
		name, actor, pin string
		lend             func(*Candidate)
	}{
		{"no acting person", "", lentPin, lendToEveryone},
		// ServesPerson admits a synthetic actor to a machine lent to
		// everyone, so a refusal here has to come from the pin's own rule:
		// these three were served by the shared plan before it.
		{"a synthetic actor, on a machine lent to everyone", "system:automation:nightly", lentPin, lendToEveryone},
		{"a connector, on a machine lent to everyone", "connector:shopify", lentPin, lendToEveryone},
		{"the operator credential's stream, on a machine lent to everyone", "cluster:operator", lentPin, lendToEveryone},
		// The automation's NAME spells the caller's short id. The share list
		// names "ursula"; the automation is not her.
		{"a synthetic actor named after a listed person", "system:automation:ursula", lentPin, lendTo([]string{"ursula"}, nil, true)},
		// Nor is it the OWNER its name spells. The own-machine arm compared
		// ids by the text after the last colon, which read
		// `system:automation:ursula` as `ursula` and handed the automation her
		// private machine with no consent at all.
		{"a synthetic actor named after a machine's owner", "system:automation:ursula", ownMachineId, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLentPinHop(t, tc.lend, nil)
			_, err := h.call(t, tc.actor, tc.pin)
			if !errors.Is(err, memqlengine.ErrFleetUnavailable) {
				t.Fatalf("want a refusal, got %v", err)
			}
			if h.studioCalls.Load() != 0 || h.ownCalls.Load() != 0 {
				t.Fatalf("lent=%d own=%d: system work must not run on a pinned machine", h.studioCalls.Load(), h.ownCalls.Load())
			}
		})
	}
}

func TestASyntheticContextCannotPinUnderAnUnprefixedName(t *testing.T) {
	// auth.ActsForNoPerson's second arm: a principal minted under a prefix
	// NamesNoPerson does not know is still nobody when the context's own
	// actor says it is Synthetic -- the cluster acting. The id alone would
	// pass for a person's.
	h := newLentPinHop(t, lendToEveryone, nil)
	ctx := auth.ContextWithAccess(authorityCtx(t, "pipeline-runner"),
		&auth.AccessContext{UserId: "pipeline-runner", Role: auth.RoleReader, Unranked: true, Synthetic: true})
	_, err := h.f.Call(ctx, memqlengine.FleetCallRequest{
		ActingUserId: "pipeline-runner", RegistrationId: lentPin, ModelId: sharedHopModel, Kind: memqlengine.FleetKindChat,
	})
	if !errors.Is(err, memqlengine.ErrFleetUnavailable) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if h.studioCalls.Load() != 0 || h.ownCalls.Load() != 0 {
		t.Fatalf("lent=%d own=%d: the cluster acting under any name must not pin a machine", h.studioCalls.Load(), h.ownCalls.Load())
	}
}

func TestAPinnedCallIsNoNarrowerThanTheGateThatAdmittedIt(t *testing.T) {
	// THE BUG CLASS, end to end. The provider path decides availability on
	// the person's catalog (fleetEntry), which has listed machines lent to
	// them since design G8; the pinned executor read the caller's own
	// machines only. So the gate said "available", the call was made, and
	// the executor refused it with the pin's sentence: the gate's extra reach
	// was dead code. This walks the seam AiChat walks -- resolve
	// `fleet:<model>` for the person, then call it under the pin.
	h := newLentPinHop(t, lendTo([]string{"ursula"}, nil, true), nil)
	registry := memqlengine.NewProviderRegistryForTest()
	registry.SetFleetInference(h.f)
	ctx := common.ContextWithFleetRegistration(authorityCtx(t, sharedHopCaller), lentPin)

	entry, ok := registry.EntryForUser(ctx, sharedHopCaller, memqlengine.FleetReferencePrefix+sharedHopModel)
	if !ok || entry == nil || !entry.Available {
		t.Fatalf("the availability gate refused a model offered by a machine lent to the caller: %+v", entry)
	}
	chat, ok := entry.Client.(common.ChatAIProvider)
	if !ok {
		t.Fatalf("a fleet entry must serve chat, got %T", entry.Client)
	}
	answer, err := chat.CallChat(ctx, []common.ChatMessage{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("the gate admitted the call and the pinned executor refused it: %v", err)
	}
	if answer != lentAnswer || h.studioCalls.Load() != 1 || h.ownCalls.Load() != 0 {
		t.Fatalf("answer %q (lent=%d own=%d): the pinned call must run on the machine it names",
			answer, h.studioCalls.Load(), h.ownCalls.Load())
	}
}

func TestAPinToYourOwnMachineTheOwnerReadMissedIsStillServed(t *testing.T) {
	// A pin the own plan has not judged goes on to the lent half, and that is
	// also where the caller's own machine is RECOVERED when the owner-scoped
	// read missed it (its row spells the owner differently). Deciding a pin
	// from the own plan alone (memql#5660) must not lose that machine.
	mine := privateMachine("mine", "v1:identity:user:alice")
	store := &sharedFleet{fakeFleet: &fakeFleet{owner: "alice"}, all: []Candidate{mine}}
	plan, err := modelRouter(t, store).PlanPinnedModel(authorityCtx(t, "alice"), "alice", "v1:worker:registration:mine", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("candidates = %v; a pin to the caller's own machine must reach it even when the owner read missed it", got)
	}
}
