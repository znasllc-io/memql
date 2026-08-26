//go:build agent

package worker

// THE MODEL-CALL HOP, tested in process (epic memql#4676, task memql#4677).
//
// Same argument as forward_hop_test.go, and the same construction: the real
// ForwardRouter wired to the real ForwardHandler through a link carrying the
// envelopes NodeService.Stream carries. A live-cluster version of this would
// be skipped on every CI lane and every developer machine, and a gate skipped
// by default cannot be what stands between a feature and the bug it prevents.
//
// The bug it prevents is specific and nasty: a machine on a sibling replica
// would report as having no model, which is indistinguishable at the surface
// from `no_local_model_available` -- the refusal this epic invented to mean
// "your fleet is asleep". A user looking at a laptop they can see is on would
// be told to wake it up.
//
// TO CONFIRM THESE ARE LOAD-BEARING: make the handler skip its registration
// check, or have relayModelDelta renumber seq, and they fail.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const hopModel = "llama3.1:8b"

// modelHop stands up both replicas plus the link, with a machine on node B
// advertising one model.
type modelHop struct {
	link  *meshLink
	store *fakeStore
	owner string
}

// serveFn is what the machine "runs": it is handed the start envelope and a
// sink for deltas, and returns the terminal outcome.
type serveFn func(ctx context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome

func newModelHop(t *testing.T, serve serveFn, mutate func(*Candidate, *workerservice.Worker)) *modelHop {
	t.Helper()
	owner := "v1:identity:user:alice"

	regB := workerservice.NewRegistry(testLogger(), fleetNow)
	w := &workerservice.Worker{
		RegistrationId: "laptop",
		OwnerUserId:    owner,
		Name:           "laptop",
		Capabilities:   []string{workerservice.CapabilityHeadless, workerservice.ModelCapability},
		Labels: map[string]string{
			workerservice.ModelLabel(hopModel):          "1",
			workerservice.RuntimeLabelPrefix + "ollama": "1",
		},
		Concurrency: map[string]uint32{workerservice.ModelCapability: 2},
	}
	// The machine's runtime, standing in for the cockpit half.
	w.SetModelCallFunc(func(ctx context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		// The stand-in runtime. `runCtx` is what a real one would park on:
		// a Cancel from the engine side cancels it, so a test can observe
		// that the stop actually crossed the hop rather than that the
		// caller merely gave up.
		runCtx, stop := context.WithCancel(ctx)
		h, emit, finish := workerservice.NewModelCallLoopback(req, func(string) { stop() })
		go func() {
			defer stop()
			finish(serve(runCtx, req, emit))
		}()
		return h, nil
	})

	cand := machine("laptop")
	cand.ConnectedNodeId = nodeB
	cand.Capabilities = []string{workerservice.CapabilityHeadless, workerservice.ModelCapability}
	cand.Labels = map[string]string{workerservice.ModelLabel(hopModel): "1"}
	if mutate != nil {
		mutate(&cand, w)
	}
	regB.Add(w)

	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{cand}, owner: owner}}
	link := &meshLink{t: t, reachable: true}
	link.handler = NewForwardHandler(regB, store, testLogger())
	link.router = newForwardRouter(link, func() (string, string) { return nodeA, "agent" }, testLogger())
	return &modelHop{link: link, store: store, owner: owner}
}

func (h *modelHop) start() *memqlv1.ModelCallStart {
	return &memqlv1.ModelCallStart{
		RequestId: "outer",
		Model:     hopModel,
		Kind:      workerservice.ModelCallKindChat,
		Messages:  []*memqlv1.ModelCallMessage{{Role: "user", Content: "hello"}},
		Limits:    &memqlv1.ModelCallLimits{TimeoutSeconds: 10, IdleTimeoutSeconds: 5, KeepaliveSeconds: 1},
	}
}

// --- the hop ----------------------------------------------------------------

func TestAModelCallReachesAMachineHeldByAnotherReplica(t *testing.T) {
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		emit(workerservice.ModelCallDelta{Seq: 1, Content: "hi "})
		emit(workerservice.ModelCallDelta{Seq: 2, Content: "there"})
		return workerservice.ModelCallOutcome{
			FinishReason: workerservice.ModelFinishStop,
			Usage:        workerservice.ModelCallUsage{InputTokens: 4, OutputTokens: 2, Known: true, Model: hopModel},
		}
	}, nil)

	var mu sync.Mutex
	var got []string
	out, err := h.link.router.ForwardModelCall(
		authorityCtx(t, h.owner), nodeB, "laptop", h.owner, h.start(), 10*time.Second,
		func(_ uint64, content string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, content)
		})
	if err != nil {
		t.Fatalf("ForwardModelCall: %v", err)
	}
	if out.RefusedBeforeStart {
		t.Fatalf("refused: %s %s -- the turn is on %s and the machine's stream is on %s; without "+
			"the forward this reads as no_local_model_available for a laptop the user can see is on",
			out.ErrorCode, out.ErrorMessage, nodeA, nodeB)
	}
	if out.End == nil {
		t.Fatal("no end envelope came back")
	}
	if out.End.GetContent() != "hi there" {
		t.Fatalf("assembled content = %q, want %q", out.End.GetContent(), "hi there")
	}
	if u := out.End.GetUsage(); !u.GetKnown() || u.GetOutputTokens() != 2 || u.GetModel() != hopModel {
		t.Fatalf("usage did not cross the hop intact: %+v", u)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "") != "hi there" {
		t.Fatalf("streamed deltas = %v -- tokens that stop at the replica holding the stream are "+
			"tokens the user never sees", got)
	}
}

// seq rides through UNTOUCHED, and the ORIGINATING side is the one that
// de-duplicates. If the relay renumbered, the originating router would be
// looking at a different sequence from the one that could reveal a gap.
func TestDuplicateAndOutOfOrderDeltasAreDroppedAcrossTheHop(t *testing.T) {
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		emit(workerservice.ModelCallDelta{Seq: 1, Content: "a"})
		emit(workerservice.ModelCallDelta{Seq: 2, Content: "b"})
		emit(workerservice.ModelCallDelta{Seq: 1, Content: "REPLAY"})
		emit(workerservice.ModelCallDelta{Seq: 2, Content: "REPLAY"})
		emit(workerservice.ModelCallDelta{Seq: 3, Content: "c"})
		return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop}
	}, nil)

	var mu sync.Mutex
	var seqs []uint64
	var got []string
	out, err := h.link.router.ForwardModelCall(
		authorityCtx(t, h.owner), nodeB, "laptop", h.owner, h.start(), 10*time.Second,
		func(seq uint64, content string) {
			mu.Lock()
			defer mu.Unlock()
			seqs = append(seqs, seq)
			got = append(got, content)
		})
	if err != nil {
		t.Fatalf("ForwardModelCall: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "") != "abc" {
		t.Fatalf("deltas = %v, want the replays dropped", got)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("seq = %v -- the relay must carry the machine's own numbering, not a renumbering", seqs)
	}
	if out.End.GetContent() != "abc" {
		t.Fatalf("assembled content = %q -- a dropped replay must not reappear through the assembly",
			out.End.GetContent())
	}
}

// A cancel on the originating side must reach the machine, or a generation
// keeps burning a laptop's GPU for work nobody is waiting for.
func TestModelCancelPropagatesAcrossTheHop(t *testing.T) {
	observed := make(chan struct{})
	stopped := make(chan struct{})
	h := newModelHop(t, func(ctx context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		close(observed)
		<-ctx.Done()
		close(stopped)
		return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishCancelled}
	}, nil)

	ctx, cancel := context.WithCancel(authorityCtx(t, h.owner))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.link.router.ForwardModelCall(ctx, nodeB, "laptop", h.owner, h.start(), 10*time.Second, nil)
	}()

	select {
	case <-observed:
	case <-time.After(3 * time.Second):
		t.Fatal("the machine never saw the call")
	}
	cancel()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reach the machine across the hop")
	}
	<-done
	if got := h.link.cancelled(); len(got) == 0 {
		t.Fatal("no ModelForwardCancel crossed the link")
	}
}

// A machine belonging to somebody else must never serve a forwarded model
// call, and the check that stops it must be on the RECEIVER: a sender that is
// buggy or compromised is exactly the case this exists for. The envelope's
// owner field is a hint; the assertion decides.
func TestAForwardedModelCallCannotReachAnotherUsersMachine(t *testing.T) {
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		t.Error("a machine owned by another user ran a model call carrying this user's prompt")
		return workerservice.ModelCallOutcome{}
	}, nil)

	// Bob asserts his own identity and names Alice's machine.
	out, err := h.link.router.ForwardModelCall(
		authorityCtx(t, "v1:identity:user:bob"), nodeB, "laptop", "v1:identity:user:bob",
		h.start(), 10*time.Second, nil)
	if err != nil {
		t.Fatalf("ForwardModelCall: %v", err)
	}
	if !out.RefusedBeforeStart {
		t.Fatal("a cross-user model call must be refused BEFORE start")
	}
	if out.ErrorCode != "registration_refused" {
		t.Fatalf("error code = %q, want registration_refused", out.ErrorCode)
	}
}

// No verifiable assertion means no forward. It refuses BEFORE START rather
// than failing the turn, so the caller can still use a machine on its own
// replica -- a working call where a hard failure would be a broken one.
func TestModelForwardWithoutAuthorityRefusesBeforeStart(t *testing.T) {
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		t.Error("a model call with no verified authority reached a machine")
		return workerservice.ModelCallOutcome{}
	}, nil)

	out, err := h.link.router.ForwardModelCall(
		context.Background(), nodeB, "laptop", h.owner, h.start(), 10*time.Second, nil)
	if err != nil {
		t.Fatalf("ForwardModelCall: %v", err)
	}
	if !out.RefusedBeforeStart || out.ErrorCode != "no_forwarded_authority" {
		t.Fatalf("out = %+v, want a pre-start no_forwarded_authority refusal", out)
	}
}

// An unreachable replica is a REFUSAL BEFORE START, which is what lets the
// router try the next machine.
func TestModelForwardToAnUnreachablePeerIsRefusedBeforeStart(t *testing.T) {
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		return workerservice.ModelCallOutcome{}
	}, nil)
	h.link.mu.Lock()
	h.link.reachable = false
	h.link.mu.Unlock()

	out, err := h.link.router.ForwardModelCall(
		authorityCtx(t, h.owner), nodeB, "laptop", h.owner, h.start(), 10*time.Second, nil)
	if err == nil {
		t.Fatal("expected the no-peer error")
	}
	if !out.RefusedBeforeStart {
		t.Fatal("an unreachable peer must be refused BEFORE start so the router can re-pick")
	}
}

// A model the machine does not advertise is refused before start rather than
// attempted: the router selected on the label, so a mismatch here means the
// fleet moved under the call.
func TestAModelTheMachineDoesNotOfferIsRefusedBeforeStart(t *testing.T) {
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		t.Error("the machine ran a model it never advertised")
		return workerservice.ModelCallOutcome{}
	}, nil)

	start := h.start()
	start.Model = "qwen2.5:72b"
	out, err := h.link.router.ForwardModelCall(
		authorityCtx(t, h.owner), nodeB, "laptop", h.owner, start, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("ForwardModelCall: %v", err)
	}
	if !out.RefusedBeforeStart || out.ErrorCode != "model_call_refused" {
		t.Fatalf("out = %+v, want a pre-start model_call_refused", out)
	}
}
