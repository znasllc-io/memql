package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

type setupAdmissionStore struct {
	mu   sync.Mutex
	row  Candidate
	read chan struct{}
}

func (s *setupAdmissionStore) WorkersForOwner(context.Context, string) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.read <- struct{}{}:
	default:
	}
	return []Candidate{s.row}, nil
}
func (*setupAdmissionStore) RoutingPolicyForOwner(context.Context, string) (*Policy, error) {
	return nil, nil
}
func (*setupAdmissionStore) TouchWorkerSelected(context.Context, string, string) error { return nil }

func TestModelSetupAdmissionRereadsAfterWaitingAndReleasesRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s := &setupAdmissionStore{row: Candidate{RegistrationId: "machine", ModelSetup: memqlengine.ModelSetup{Revision: "one", Choices: map[string]string{"conversation": "chat"}}}, read: make(chan struct{}, 2)}
	h := &ForwardHandler{store: s}
	w := &workerservice.Worker{OwnerUserId: "owner"}
	release, err := w.AcquireModelSetup(ctx)
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		_, done, err := h.admitModelSetup(ctx, w, "owner", "machine", &memqlv1.ModelCallStart{Model: "chat"})
		if done != nil {
			done()
		}
		result <- err
	}()
	select {
	case <-s.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	s.mu.Lock()
	s.row.ModelSetup = memqlengine.ModelSetup{Revision: "two", Choices: map[string]string{"conversation": "other"}}
	s.mu.Unlock()
	release()
	require.ErrorContains(t, <-result, "no longer selected")
	// A refusal must return the memory slot, and an omitted context is bounded.
	start := &memqlv1.ModelCallStart{Model: "other"}
	_, release, err = h.admitModelSetup(ctx, w, "owner", "machine", start)
	require.NoError(t, err)
	require.EqualValues(t, memqlengine.ModelSetupContextLimit, start.GetParams().GetContextTokens())
	release()
	start.Params.ContextTokens = memqlengine.ModelSetupContextLimit + 1
	_, _, err = h.admitModelSetup(ctx, w, "owner", "machine", start)
	require.ErrorContains(t, err, "32K")
	release, err = w.AcquireModelSetup(ctx)
	require.NoError(t, err)
	release()
}

// The row as fleetClearModelSetup leaves it: an empty block, with no revision.
func clearedModelSetup() memqlengine.ModelSetup {
	return memqlengine.ModelSetupFromRow(map[string]any{})
}

// A machine returned to automatic is one where the saved setup's three limits
// are gone on the replica holding the stream: the choices, the 32K ceiling, and
// the single setup slot.
func TestAMachineReturnedToAutomaticAdmitsAnyModelAndALargerContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	saved := memqlengine.ModelSetup{Revision: "one", Choices: map[string]string{"conversation": "chat"}}
	s := &setupAdmissionStore{row: Candidate{RegistrationId: "machine", ModelSetup: saved}, read: make(chan struct{}, 8)}
	h := &ForwardHandler{store: s}
	w := &workerservice.Worker{OwnerUserId: "owner"}
	const above = memqlengine.ModelSetupContextLimit * 2
	wide := func(model string) *memqlv1.ModelCallStart {
		return &memqlv1.ModelCallStart{Model: model, Params: &memqlv1.ModelCallParams{ContextTokens: above}}
	}

	_, _, err := h.admitModelSetup(ctx, w, "owner", "machine", &memqlv1.ModelCallStart{Model: "other"})
	require.ErrorContains(t, err, "no longer selected")
	_, _, err = h.admitModelSetup(ctx, w, "owner", "machine", wide("chat"))
	require.ErrorContains(t, err, "32K")

	s.mu.Lock()
	s.row.ModelSetup = clearedModelSetup()
	s.mu.Unlock()

	// Holding the slot proves nothing waits for it: a saved setup's admission
	// would block here until the context ran out.
	slot, err := w.AcquireModelSetup(ctx)
	require.NoError(t, err)
	defer slot()

	start := wide("other")
	_, release, err := h.admitModelSetup(ctx, w, "owner", "machine", start)
	require.NoError(t, err, "the model the setup left out is admitted again")
	release()
	require.EqualValues(t, above, start.GetParams().GetContextTokens(), "a context above 32K goes through as asked")

	// An omitted context stays the machine's own to decide, not the setup's
	// budget, which only a saved setup reserved.
	omitted := &memqlv1.ModelCallStart{Model: "chat"}
	_, release, err = h.admitModelSetup(ctx, w, "owner", "machine", omitted)
	require.NoError(t, err)
	release()
	require.Zero(t, omitted.GetParams().GetContextTokens())
}

// A call queued behind a saved setup's slot rereads after the wait, so the owner
// returning the machine to automatic reaches it too.
func TestACallWaitingOnTheSetupSlotSeesTheMachineReturnToAutomatic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s := &setupAdmissionStore{row: Candidate{RegistrationId: "machine", ModelSetup: memqlengine.ModelSetup{Revision: "one", Choices: map[string]string{"conversation": "chat"}}}, read: make(chan struct{}, 2)}
	h := &ForwardHandler{store: s}
	w := &workerservice.Worker{OwnerUserId: "owner"}
	held, err := w.AcquireModelSetup(ctx)
	require.NoError(t, err)
	type admitted struct {
		start *memqlv1.ModelCallStart
		err   error
	}
	result := make(chan admitted, 1)
	go func() {
		start := &memqlv1.ModelCallStart{Model: "other", Params: &memqlv1.ModelCallParams{ContextTokens: memqlengine.ModelSetupContextLimit + 1}}
		_, done, err := h.admitModelSetup(ctx, w, "owner", "machine", start)
		if done != nil {
			done()
		}
		result <- admitted{start, err}
	}()
	select {
	case <-s.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	s.mu.Lock()
	s.row.ModelSetup = clearedModelSetup()
	s.mu.Unlock()
	held()
	got := <-result
	require.NoError(t, got.err)
	require.EqualValues(t, memqlengine.ModelSetupContextLimit+1, got.start.GetParams().GetContextTokens())
}

func TestARoutePlanOffersAnyModelOnAMachineReturnedToAutomatic(t *testing.T) {
	const above = memqlengine.ModelSetupContextLimit * 2
	plan := func(setup memqlengine.ModelSetup) RoutePlan {
		return RoutePlan{Policy: DefaultPolicy(), Total: 1, Candidates: []Candidate{{
			RegistrationId: "laptop",
			ModelSetup:     setup,
			Labels: map[string]string{
				workerservice.ModelLabel("chat"):  "ctx=131072,tools=1",
				workerservice.ModelLabel("other"): "ctx=131072,tools=1",
			},
		}}}
	}
	saved := memqlengine.ModelSetup{Revision: "one", Choices: map[string]string{"tools": "chat"}}
	require.Empty(t, narrowToModel(plan(saved), "other", ModelNeeds{Tools: true}).Candidates)
	require.Empty(t, narrowToModel(plan(saved), "chat", ModelNeeds{Tools: true, MinContextWindow: above}).Candidates)

	require.Len(t, narrowToModel(plan(clearedModelSetup()), "other", ModelNeeds{Tools: true}).Candidates, 1)
	require.Len(t, narrowToModel(plan(clearedModelSetup()), "chat", ModelNeeds{Tools: true, MinContextWindow: above}).Candidates, 1)
}
