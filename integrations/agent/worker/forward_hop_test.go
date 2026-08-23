//go:build agent

package worker

// THE CROSS-NODE HOP, tested in process (memql#4352).
//
// The live-cluster version of this belongs in test/clustere2e and would be
// skipped everywhere a two-replica cluster is not running -- which is every CI
// lane and every developer's machine. A gate that is skipped by default cannot
// be the thing standing between this feature and the bug it exists to prevent,
// which is the argument test/clustere2e/automation_run_routing_test.go makes at
// length about its own subject. So the hop is gated HERE, deterministically,
// by wiring the REAL ForwardRouter to the REAL ForwardHandler through a link
// that carries the same envelopes NodeService.Stream carries.
//
// TO CONFIRM IT IS LOAD-BEARING: make attemptRemote dispatch locally instead
// of forwarding, or make the handler skip its registration check, and these
// fail. If they pass either way they are worthless.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const (
	nodeA = "agent-1" // serves the turn
	nodeB = "agent-2" // holds the machine's stream
)

// meshLink joins two in-process replicas the way NodeService joins two pods:
// a NodeClientMessage from A reaches B's handler, and every NodeServerMessage
// B sends is routed back into A's sinks by request id.
type meshLink struct {
	t       *testing.T
	handler *ForwardHandler
	router  *ForwardRouter

	mu        sync.Mutex
	reachable bool
	cancels   []string
	wg        sync.WaitGroup
}

func (l *meshLink) Send(nodeId string, msg *nodev1.NodeClientMessage) bool {
	l.mu.Lock()
	reachable := l.reachable
	l.mu.Unlock()
	if !reachable || nodeId != nodeB {
		return false
	}
	switch payload := msg.GetPayload().(type) {
	case *nodev1.NodeClientMessage_WorkerForwardRequest:
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handler.HandleForwardedRequest(context.Background(), payload.WorkerForwardRequest, l.back)
		}()
	case *nodev1.NodeClientMessage_WorkerForwardCancel:
		l.mu.Lock()
		l.cancels = append(l.cancels, payload.WorkerForwardCancel.GetRequestId())
		l.mu.Unlock()
		l.handler.CancelForwardedRequest(context.Background(), payload.WorkerForwardCancel.GetRequestId())
	}
	return true
}

// back is B's `send`: the return leg.
func (l *meshLink) back(msg *nodev1.NodeServerMessage) error {
	switch payload := msg.GetPayload().(type) {
	case *nodev1.NodeServerMessage_WorkerForwardResponse:
		l.router.Dispatch(payload.WorkerForwardResponse)
	case *nodev1.NodeServerMessage_WorkerForwardStream:
		l.router.DispatchStream(payload.WorkerForwardStream)
	}
	return nil
}

func (l *meshLink) cancelled() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.cancels))
	copy(out, l.cancels)
	return out
}

// authorityCtx builds a context carrying a verifiable assertion for the owner,
// which is what the sender re-asserts and the receiver checks the machine
// against.
func authorityCtx(t *testing.T, userId string) context.Context {
	t.Helper()
	authority, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{UserId: userId, PrimaryEmail: "owner@example.com", Role: auth.RoleWriter},
		"", "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser: %v", err)
	}
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		t.Fatalf("VerifyForwardedAuthority: %v", err)
	}
	return auth.BindForwardedContext(context.Background(), authority.Principal().Claims, access, authority)
}

// hop stands up both replicas plus the link between them.
type hop struct {
	link     *meshLink
	registry *workerservice.Registry // node B's -- it holds the stream
	store    *fakeStore
	dispatch *Dispatcher // node A's
	owner    string
}

func newHop(t *testing.T, machineDispatch workerservice.DispatchFunc, mutate func(*Candidate)) *hop {
	t.Helper()
	owner := "v1:identity:user:alice"

	// Node B: the registry holding the machine's stream.
	regB := workerservice.NewRegistry(testLogger(), fleetNow)
	w := &workerservice.Worker{
		RegistrationId: "laptop",
		OwnerUserId:    owner,
		Name:           "laptop",
		Capabilities:   []string{workerservice.CapabilityHeadless},
		Concurrency:    map[string]uint32{workerservice.CapabilityHeadless: 2},
	}
	w.SetDispatchFunc(machineDispatch, func() {})
	regB.Add(w)

	cand := machine("laptop")
	cand.ConnectedNodeId = nodeB
	if mutate != nil {
		mutate(&cand)
	}
	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{cand}, owner: owner}}

	link := &meshLink{t: t, reachable: true}
	link.handler = NewForwardHandler(regB, store, testLogger())
	link.router = newForwardRouter(link, func() (string, string) { return nodeA, "agent" }, testLogger())

	// Node A: serves the turn, holds NO stream for the machine.
	d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), nodeA, link.router)
	return &hop{link: link, registry: regB, store: store, dispatch: d, owner: owner}
}

func (h *hop) request() Request {
	req := approvedRequest()
	req.OwnerUserId = h.owner
	return req
}

// --- the hop ----------------------------------------------------------------

func TestATurnOnOneReplicaReachesAMachineHeldByAnother(t *testing.T) {
	ran := ""
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		ran = d.GetAction()
		return okResult(d.GetCallId()), nil
	}, nil)

	res, err := h.dispatch.Dispatch(authorityCtx(t, h.owner), h.request())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v -- the turn is on %s and the machine's stream is on %s; without the "+
			"forward this is the no_worker_available a user sees for a laptop they can see is on",
			res, nodeA, nodeB)
	}
	if ran != "fs_read" {
		t.Fatalf("the machine ran %q, want the original action", ran)
	}
	if got := h.store.lastInvocation(t).WorkerId; got != "laptop" {
		t.Fatalf("invocation.workerId = %q, want the machine that ran it", got)
	}
}

func TestStreamedChunksCrossTheHop(t *testing.T) {
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		onChunk(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StdoutChunk{StdoutChunk: []byte("one")}})
		onChunk(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StderrChunk{StderrChunk: []byte("two")}})
		return okResult(d.GetCallId()), nil
	}, nil)

	var mu sync.Mutex
	var got []string
	req := h.request()
	req.OnStreamChunk = func(c *nodev1.WorkerForwardStream) {
		mu.Lock()
		defer mu.Unlock()
		switch p := c.GetPayload().(type) {
		case *nodev1.WorkerForwardStream_StdoutChunk:
			got = append(got, "out:"+string(p.StdoutChunk))
		case *nodev1.WorkerForwardStream_StderrChunk:
			got = append(got, "err:"+string(p.StderrChunk))
		}
	}
	if _, err := h.dispatch.Dispatch(authorityCtx(t, h.owner), req); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.link.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "out:one,err:two" {
		t.Fatalf("chunks = %v, want both, in order -- output that stops at the replica holding "+
			"the stream is output the user never sees", got)
	}
}

func TestCancelPropagatesAcrossTheHop(t *testing.T) {
	release := make(chan struct{})
	observed := make(chan struct{})
	h := newHop(t, func(ctx context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		close(observed)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return okResult(d.GetCallId()), nil
		}
	}, nil)
	defer close(release)

	ctx, cancel := context.WithCancel(authorityCtx(t, h.owner))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.dispatch.Dispatch(ctx, h.request())
	}()
	<-observed
	cancel()
	<-done

	if got := h.link.cancelled(); len(got) != 1 {
		t.Fatalf("cancels sent = %v, want exactly one -- a cancelled turn must free the machine's "+
			"concurrency slot on the replica that holds it, not wait out the timeout", got)
	}
}

// --- what the receiver re-checks -------------------------------------------

func TestTheReceiverRefusesARevokedMachine(t *testing.T) {
	// Revocation is a row, and the sender read it up to one heartbeat ago.
	// "Revoked while a turn was in flight" is precisely the window this check
	// exists for, so the fixture revokes it only on the receiving side's read.
	h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("a revoked machine must never be dispatched to")
		return nil, nil
	}, nil)
	h.store.machines[0].RevokedAt = at(1)

	res, _ := h.dispatch.Dispatch(authorityCtx(t, h.owner), h.request())
	if res.OK {
		t.Fatal("want a refusal")
	}
	if res.ErrorCode != "no_worker_available" {
		t.Fatalf("errorCode = %q, want the router to drop it before it ever forwards", res.ErrorCode)
	}
}

func TestTheReceiverRefusesAMachineTheAssertionDoesNotOwn(t *testing.T) {
	// The envelope names a registration; the verified authority names a
	// different person. Without this check a replica could reach any machine
	// whose id it knew.
	h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("a machine belonging to someone else must never be dispatched to")
		return nil, nil
	}, nil)

	req := &nodev1.WorkerForwardRequest{
		RequestId:      "r1",
		RegistrationId: "laptop",
		OwnerUserId:    "v1:identity:user:mallory",
		Capability:     workerservice.CapabilityHeadless,
		Tool:           "workerHost",
		Action:         "fs_read",
		Authority:      authorityProtoFor(t, "v1:identity:user:mallory"),
	}
	var got *nodev1.WorkerForwardResponse
	h.link.handler.HandleForwardedRequest(context.Background(), req, func(m *nodev1.NodeServerMessage) error {
		if r := m.GetWorkerForwardResponse(); r != nil {
			got = r
		}
		return nil
	})
	if got == nil {
		t.Fatal("the receiver must answer rather than drop: the sender is parked on this reply")
	}
	if got.GetErrorCode() != "registration_refused" {
		t.Fatalf("errorCode = %q, want registration_refused", got.GetErrorCode())
	}
	if !got.GetRefusedBeforeStart() {
		t.Fatal("a refusal that ran nothing must SAY it ran nothing, or the sender cannot try another machine")
	}
}

func TestAnUnverifiableAuthorityIsRefusedBeforeStart(t *testing.T) {
	h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("an envelope with no verifiable assertion must never reach the machine")
		return nil, nil
	}, nil)
	var got *nodev1.WorkerForwardResponse
	h.link.handler.HandleForwardedRequest(context.Background(),
		&nodev1.WorkerForwardRequest{RequestId: "r1", RegistrationId: "laptop", Capability: workerservice.CapabilityHeadless},
		func(m *nodev1.NodeServerMessage) error {
			if r := m.GetWorkerForwardResponse(); r != nil {
				got = r
			}
			return nil
		})
	if got == nil || got.GetErrorCode() != "forwarded_authority_refused" || !got.GetRefusedBeforeStart() {
		t.Fatalf("response = %+v, want a pre-start refusal naming the authority", got)
	}
}

// --- the unreachable peer ---------------------------------------------------

func TestAnUnreachablePeerIsARefusalNotALocalDispatch(t *testing.T) {
	h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("nothing should reach the machine when its replica is unreachable")
		return nil, nil
	}, nil)
	h.link.mu.Lock()
	h.link.reachable = false
	h.link.mu.Unlock()

	res, _ := h.dispatch.Dispatch(authorityCtx(t, h.owner), h.request())
	if res.OK {
		t.Fatal("want a refusal -- this replica holds no stream for the machine, so a local " +
			"dispatch is not a degraded path, it is a call that cannot work")
	}
	if got := h.store.lastInvocation(t).Routing["attempts"]; got != 1 {
		t.Fatalf("attempts = %v, want 1", got)
	}
}

func TestAfterTheMachineReconnectsHereTheDispatchIsLocal(t *testing.T) {
	// The machine moved to this replica. The row's connectedNodeId now names
	// us, so the forward must not be used at all.
	h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("a machine connected HERE must not be forwarded")
		return nil, nil
	}, nil)
	h.store.machines[0].ConnectedNodeId = nodeA

	localRan := false
	localReg := workerservice.NewRegistry(testLogger(), fleetNow)
	liveMachine(t, localReg, "laptop", 2, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		localRan = true
		return okResult(d.GetCallId()), nil
	})
	d := newTestDispatcher(t, h.store, localReg, nodeA, h.link.router)

	res, err := d.Dispatch(authorityCtx(t, h.owner), h.request())
	if err != nil || !res.OK {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	if !localRan {
		t.Fatal("want the local stream used")
	}
}

// authorityProtoFor builds a wire assertion for an arbitrary subject.
func authorityProtoFor(t *testing.T, userId string) *nodev1.ForwardedAuthority {
	t.Helper()
	a, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{UserId: userId, PrimaryEmail: "x@example.com", Role: auth.RoleWriter},
		"", "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser: %v", err)
	}
	return node.ForwardedAuthorityToProto(a, nodeA, "agent")
}
