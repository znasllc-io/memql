package workbench

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// forward_pipeline_test.go -- epic memql#5478 (#5493): the four pipeline
// actions on the workbench's forward handler, and the agent-side forward that
// watches the replica it is waiting on.
//
// The two tests whose subject is a CANCEL run over a real NodeService stream on
// loopback: an agent replica's ForwardRouter and WorkerDialer on one side, a
// workbench replica's NodeServer and ForwardHandler on the other. The property
// each pins lives in the hop rather than in either half. "A cancel reaches a
// running step" is false for a handler that blocks its receive loop, and a test
// that calls the handler directly never has a receive loop to block; "no cancel
// was sent" means nothing unless the same stream is shown to carry one.

// pipelineActions is the four actions, in one place for the table tests.
var pipelineActions = []string{PipelineStepAction, PipelineStatusAction, PipelineAckAction, PipelineCancelAction, PipelineReadinessAction}

// The outcomes the fake runner answers a step with: released by the test, or
// ended by its context -- the two ways a real step ends.
const (
	fakeStepSucceeded = `{"status":"succeeded","exitCode":0}`
	fakeStepCancelled = `{"status":"cancelled","failure":{"code":"pipeline_step_cancelled"}}`
)

// fakePipelineRunner stands in for the pipelinesteps runner behind app/'s
// adapter. It records every call with the args it was handed (the handler's
// contract is JSON in, verbatim), and RunStep parks until the test finishes
// the step or its context ends.
type fakePipelineRunner struct {
	mu    sync.Mutex
	calls []string
	steps []*fakeStep

	started chan *fakeStep

	statusReply []byte
	statusCode  string
	ackCode     string
	cancelReply []byte
	cancelCode  string

	// statusHold, when set, holds every Status until it is closed or the
	// call's context ends: a runner whose API server does not answer.
	// statusEntered is told each Status that began.
	statusHold    chan struct{}
	statusEntered chan struct{}
}

func (f *fakePipelineRunner) Readiness(context.Context) ([]byte, string) {
	f.record(PipelineReadinessAction, nil)
	return []byte(`{"available":true,"isolation":"not_proven"}`), ""
}

// fakeStep is one RunStep in progress: the context the handler ran it under,
// and the switch that lets it finish.
type fakeStep struct {
	ctx     context.Context
	release chan struct{}
	once    sync.Once
}

// finish lets the step succeed. Safe to call more than once.
func (s *fakeStep) finish() { s.once.Do(func() { close(s.release) }) }

// newFakePipelineRunner finishes every step it was handed when the test ends,
// whatever the test did. A step left parked would hold its goroutine forever,
// and on the hop it would hold the workbench's receive loop too if a handler
// ever ran one inline -- wedging the node server's shutdown, so that a failing
// test hangs instead of reporting.
func newFakePipelineRunner(t *testing.T) *fakePipelineRunner {
	f := &fakePipelineRunner{
		started:     make(chan *fakeStep, 8),
		statusReply: []byte(`{"state":"running"}`),
		cancelReply: []byte(`{"jobsDeleted":2}`),
	}
	t.Cleanup(f.finishAll)
	return f
}

func (f *fakePipelineRunner) finishAll() {
	f.mu.Lock()
	steps := append([]*fakeStep(nil), f.steps...)
	f.mu.Unlock()
	for _, s := range steps {
		s.finish()
	}
}

func (f *fakePipelineRunner) record(action string, args []byte) {
	f.mu.Lock()
	f.calls = append(f.calls, action+" "+string(args))
	f.mu.Unlock()
}

func (f *fakePipelineRunner) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePipelineRunner) RunStep(ctx context.Context, argsJSON []byte) []byte {
	f.record(PipelineStepAction, argsJSON)
	step := &fakeStep{ctx: ctx, release: make(chan struct{})}
	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	f.started <- step
	select {
	case <-step.release:
		return []byte(fakeStepSucceeded)
	case <-ctx.Done():
		return []byte(fakeStepCancelled)
	}
}

func (f *fakePipelineRunner) Status(ctx context.Context, argsJSON []byte) ([]byte, string) {
	f.record(PipelineStatusAction, argsJSON)
	f.mu.Lock()
	hold, entered := f.statusHold, f.statusEntered
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	return f.statusReply, f.statusCode
}

func (f *fakePipelineRunner) Ack(_ context.Context, argsJSON []byte) string {
	f.record(PipelineAckAction, argsJSON)
	return f.ackCode
}

func (f *fakePipelineRunner) CancelRun(_ context.Context, argsJSON []byte) ([]byte, string) {
	f.record(PipelineCancelAction, argsJSON)
	return f.cancelReply, f.cancelCode
}

// awaitStep returns the next step the runner was handed.
func (f *fakePipelineRunner) awaitStep(t *testing.T, what string) *fakeStep {
	t.Helper()
	select {
	case step := <-f.started:
		return step
	case <-time.After(5 * time.Second):
		t.Fatalf("the runner was never handed %s", what)
		return nil
	}
}

// replyLog collects what a handler sends, from whichever goroutine sends it: a
// step's reply comes from the goroutine the handler started, not the caller.
type replyLog struct {
	ch chan *nodev1.NodeServerMessage
}

func newReplyLog() *replyLog { return &replyLog{ch: make(chan *nodev1.NodeServerMessage, 16)} }

func (r *replyLog) send(m *nodev1.NodeServerMessage) error {
	r.ch <- m
	return nil
}

// next returns the next reply, which must be a WorkbenchForwardResponse
// correlated to its own request.
func (r *replyLog) next(t *testing.T, what string) *nodev1.WorkbenchForwardResponse {
	t.Helper()
	select {
	case m := <-r.ch:
		resp := m.GetWorkbenchForwardResponse()
		if resp == nil {
			t.Fatalf("%s: the handler sent %T, want a WorkbenchForwardResponse", what, m.GetPayload())
		}
		if m.GetCorrelateTo() != resp.GetRequestId() {
			t.Fatalf("%s: correlateTo %q does not name the request %q it answers", what, m.GetCorrelateTo(), resp.GetRequestId())
		}
		return resp
	case <-time.After(5 * time.Second):
		t.Fatalf("no reply for %s. The agent's executor parks on one; a dropped message is a step that "+
			"waits out its deadline rather than one that was answered.", what)
		return nil
	}
}

// none fails if anything has been sent.
func (r *replyLog) none(t *testing.T, why string) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("%s, but the handler sent %v", why, m)
	default:
	}
}

func systemAuthority(t *testing.T) *nodev1.ForwardedAuthority {
	t.Helper()
	a, err := auth.ForwardedAuthorityForSystem("pipelines:agent-a", time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForSystem: %v", err)
	}
	return node.ForwardedAuthorityToProto(a, "agent-a", "agent")
}

func personAuthority(t *testing.T, class string, role auth.Role) *nodev1.ForwardedAuthority {
	t.Helper()
	a, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{UserId: "v1:identity:user:alice", PrimaryEmail: "alice@example.com", Role: role},
		class, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser(%s): %v", class, err)
	}
	return node.ForwardedAuthorityToProto(a, "agent-a", "agent")
}

func pipelineForward(requestId, action, args string, authority *nodev1.ForwardedAuthority) *nodev1.WorkbenchForwardRequest {
	return &nodev1.WorkbenchForwardRequest{
		RequestId: requestId,
		RunId:     "v1:pipelines:run:r1",
		Action:    action,
		ArgsJson:  []byte(args),
		Authority: authority,
	}
}

// pipelineHandler is a workbench-side handler with runner installed, or with
// none when runner is nil.
func pipelineHandler(runner PipelineRunner) *ForwardHandler {
	h := NewForwardHandler(NewIntegration(buildLogger()), buildLogger())
	if runner != nil {
		h.SetPipelineRunner(runner)
	}
	return h
}

// trackedRequests counts the requests the handler can still cancel.
func trackedRequests(h *ForwardHandler) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inflight)
}

func awaitCondition(t *testing.T, cond func() bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// The handler
// ---------------------------------------------------------------------------

// TestPipelineStepDoesNotBlockTheReceiveLoop is the property the asynchronous
// path exists for.
//
// HandleForwardedRequest is called on the peer stream's RECEIVE loop
// (component/node/stream_handler.go). A step runs for as long as its Job --
// twenty minutes by default -- so a step answered inline would stop this node
// reading heartbeats, event forwards and every other forward from that peer for
// the whole time, and the WorkbenchForwardCancel that could end it waits behind
// it on the same loop. So the call returns while the step still runs, the loop
// is free for the next message, and the reply arrives later through the same
// send.
func TestPipelineStepDoesNotBlockTheReceiveLoop(t *testing.T) {
	runner := newFakePipelineRunner(t)
	h := pipelineHandler(runner)
	out := newReplyLog()

	stepReq := pipelineForward("step-1", PipelineStepAction, `{"stepKey":"test"}`, systemAuthority(t))
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.HandleForwardedRequest(context.Background(), stepReq, out.send)
	}()
	step := runner.awaitStep(t, "a SYSTEM-class pipelineStep, with a runner installed")
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleForwardedRequest is still inside RunStep. It runs on the peer stream's receive loop, so a " +
			"step answered inline stops this node reading that peer -- heartbeats, event forwards, and the cancel " +
			"for this very step -- for as long as the step runs.")
	}
	out.none(t, "the step is still running")

	// The loop is free, and the proof is the next message on it: a status
	// forward is answered while the step still runs.
	h.HandleForwardedRequest(context.Background(),
		pipelineForward("status-1", PipelineStatusAction, `{"jobName":"mp-1"}`, systemAuthority(t)), out.send)
	if got := out.next(t, "the status forward sent while the step runs"); got.GetRequestId() != "status-1" {
		t.Fatalf("the next reply answers %q, want the status forward -- the step has not finished", got.GetRequestId())
	}

	step.finish()
	resp := out.next(t, "the step, once it finished")
	if resp.GetRequestId() != "step-1" {
		t.Fatalf("reply answers %q, want step-1", resp.GetRequestId())
	}
	if string(resp.GetPayloadJson()) != fakeStepSucceeded || resp.GetErrorCode() != "" {
		t.Fatalf("reply = payload %s / errorCode %q, want the runner's outcome verbatim and no error code",
			resp.GetPayloadJson(), resp.GetErrorCode())
	}
	if n := trackedRequests(h); n != 0 {
		t.Fatalf("%d request(s) still tracked after the step answered. The step's goroutine owns its entry and "+
			"must release it: a leaked entry is a cancel that reaches nothing, held forever.", n)
	}
}

// TestPipelineActionsNeedSystemAuthority is the class gate -- the build
// entry's rule, applied to the four pipeline entries for the build entry's
// reason. Only this cluster's own engine can mint a SYSTEM-class assertion, and
// only the engine's executor drives pipeline steps; a person's session, however
// privileged, reaches none of the four, and a refusal runs nothing.
func TestPipelineActionsNeedSystemAuthority(t *testing.T) {
	refused := []struct {
		name      string
		authority func(*testing.T) *nodev1.ForwardedAuthority
	}{
		// What every agent tool-loop forward carries, which is what makes it the
		// case worth pinning.
		{"an ordinary user session", func(t *testing.T) *nodev1.ForwardedAuthority {
			return personAuthority(t, auth.ForwardedClassUser, auth.RoleWriter)
		}},
		// The most powerful credentials a person can hold: the gate is on the
		// CLASS, not the role.
		{"an operator stream", func(t *testing.T) *nodev1.ForwardedAuthority {
			return personAuthority(t, auth.ForwardedClassOperator, auth.RoleOwner)
		}},
		{"the auth-disabled dev shim", func(t *testing.T) *nodev1.ForwardedAuthority {
			return personAuthority(t, auth.ForwardedClassLocalDev, auth.RoleOwner)
		}},
		{"no assertion at all", func(*testing.T) *nodev1.ForwardedAuthority { return nil }},
	}
	wantPayload := map[string]string{
		PipelineReadinessAction: `{"available":true,"isolation":"not_proven"}`,
		PipelineStepAction:      fakeStepSucceeded,
		PipelineStatusAction:    `{"state":"running"}`,
		PipelineAckAction:       "",
		PipelineCancelAction:    `{"jobsDeleted":2}`,
	}

	for _, action := range pipelineActions {
		t.Run(action, func(t *testing.T) {
			runner := newFakePipelineRunner(t)
			h := pipelineHandler(runner)
			out := newReplyLog()

			for _, who := range refused {
				h.HandleForwardedRequest(context.Background(),
					pipelineForward("refused", action, `{"runId":"r1"}`, who.authority(t)), out.send)
				if got := out.next(t, who.name).GetErrorCode(); got != "forwarded_authority_refused" {
					t.Errorf("%s: errorCode = %q, want forwarded_authority_refused", who.name, got)
				}
			}
			if calls := runner.called(); len(calls) != 0 {
				t.Fatalf("a refused forward reached the runner: %v", calls)
			}

			// THE REACHABLE POSITIVE. The same request under the engine's own
			// assertion reaches the runner -- so the refusals above are about the
			// class, not a handler that refuses everything -- and the runner is
			// handed the args exactly as forwarded.
			h.HandleForwardedRequest(context.Background(),
				pipelineForward("admitted", action, `{"runId":"r1"}`, systemAuthority(t)), out.send)
			if action == PipelineStepAction {
				runner.awaitStep(t, "the admitted step").finish()
			}
			resp := out.next(t, "the admitted forward")
			if resp.GetErrorCode() != "" {
				t.Fatalf("the engine's own assertion was refused: %s: %s", resp.GetErrorCode(), resp.GetErrorMessage())
			}
			if got := string(resp.GetPayloadJson()); got != wantPayload[action] {
				t.Errorf("payload = %q, want the runner's JSON %q", got, wantPayload[action])
			}
			wantCall := action + ` {"runId":"r1"}`
			if action == PipelineReadinessAction {
				wantCall = action + " "
			}
			if calls := runner.called(); len(calls) != 1 || calls[0] != wantCall {
				t.Errorf("runner calls = %q, want exactly one %s with the forwarded args", calls, action)
			}
		})
	}
}

// TestPipelineRepliesMirrorTheRunnersRefusal: a refusal the runner answers
// with rides the response's error_code, so the agent's executor reads
// success or failure off the envelope without parsing a payload whose shape
// depends on the action -- and the runner's JSON still arrives beside it.
func TestPipelineRepliesMirrorTheRunnersRefusal(t *testing.T) {
	runner := newFakePipelineRunner(t)
	runner.statusCode = "cluster_api_unreachable"
	runner.ackCode = "cluster_api_unreachable"
	runner.cancelCode = "cluster_api_unreachable"
	runner.cancelReply = []byte(`{"jobsDeleted":1}`)
	h := pipelineHandler(runner)
	out := newReplyLog()

	for _, tc := range []struct {
		action, wantPayload string
	}{
		{PipelineStatusAction, `{"state":"running"}`},
		{PipelineAckAction, ""},
		{PipelineCancelAction, `{"jobsDeleted":1}`},
	} {
		h.HandleForwardedRequest(context.Background(),
			pipelineForward(tc.action, tc.action, `{}`, systemAuthority(t)), out.send)
		resp := out.next(t, tc.action)
		if resp.GetErrorCode() != "cluster_api_unreachable" {
			t.Errorf("%s: errorCode = %q, want the runner's cluster_api_unreachable", tc.action, resp.GetErrorCode())
		}
		if resp.GetErrorMessage() == "" {
			t.Errorf("%s: a refusal with no message names nothing for the log that records it", tc.action)
		}
		if got := string(resp.GetPayloadJson()); got != tc.wantPayload {
			t.Errorf("%s: payload = %q, want %q", tc.action, got, tc.wantPayload)
		}
	}
}

// TestPipelineActionsWithNoRunnerAnswerNotConfigured: a workbench node with no
// runner -- no in-cluster API access, or no clone image configured -- answers
// every pipeline action promptly with pipelines_not_configured, having run
// nothing. A dropped message would park the agent's executor until its
// deadline, and a generic dispatch error would read as the step failing rather
// than as this node being unable to run steps at all.
func TestPipelineActionsWithNoRunnerAnswerNotConfigured(t *testing.T) {
	h := pipelineHandler(nil)
	out := newReplyLog()
	for _, action := range pipelineActions {
		h.HandleForwardedRequest(context.Background(),
			pipelineForward(action, action, `{"stepKey":"test"}`, systemAuthority(t)), out.send)
		if got := out.next(t, action).GetErrorCode(); got != "pipelines_not_configured" {
			t.Errorf("%s: errorCode = %q, want pipelines_not_configured", action, got)
		}
	}
	if n := trackedRequests(h); n != 0 {
		t.Fatalf("%d request(s) tracked by a node that ran nothing", n)
	}
}

// TestAPipelineStepOutlivesTheStreamItArrivedOn. The runner reads a done
// context as a CANCEL -- it deletes the step's Job -- so a step run under the
// stream's context would be deleted by anything that ends the stream: a mesh
// flap, the agent replica restarting, this workbench draining for a deploy.
// Each of those is exactly when the Job has to survive, because the agent
// re-forwards and the next replica adopts the Job by name. The step keeps the
// request's values (the verified authority travels with it) and drops only its
// cancellation; WorkbenchForwardCancel is the one thing that ends it.
func TestAPipelineStepOutlivesTheStreamItArrivedOn(t *testing.T) {
	runner := newFakePipelineRunner(t)
	h := pipelineHandler(runner)
	out := newReplyLog()

	stream, endStream := context.WithCancel(context.Background())
	h.HandleForwardedRequest(stream,
		pipelineForward("step-1", PipelineStepAction, `{"stepKey":"test"}`, systemAuthority(t)), out.send)
	step := runner.awaitStep(t, "the step")

	endStream()
	if err := step.ctx.Err(); err != nil {
		t.Fatalf("the stream ending ended the step (%v). The runner deletes a Job whose context is done, so a "+
			"deploy or a mesh flap would delete running steps instead of leaving them to be adopted.", err)
	}
	if a, ok := auth.ForwardedAuthorityFromContext(step.ctx); !ok || a.CredentialClass != auth.ForwardedClassSystem {
		t.Fatalf("the step runs without the verified assertion (%+v, %v); it must keep the request's values "+
			"and drop only its cancellation", a, ok)
	}

	// The reachable positive: the cancel does end it, and the cancelled
	// outcome is still answered.
	h.CancelForwardedRequest(context.Background(), "step-1")
	select {
	case <-step.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("CancelForwardedRequest did not reach the running step")
	}
	if resp := out.next(t, "the cancelled step"); string(resp.GetPayloadJson()) != fakeStepCancelled {
		t.Fatalf("payload = %s, want the runner's cancelled outcome", resp.GetPayloadJson())
	}
}

// ---------------------------------------------------------------------------
// The hop
// ---------------------------------------------------------------------------

const hopWorkbenchId = "workbench-a"

// pipelineHop is two replicas joined by a real NodeService stream on loopback:
// an agent replica holding the ForwardRouter under test and the WorkerDialer
// production uses to reach a workbench, and a workbench replica whose
// NodeServer hands every forward to a real ForwardHandler. Nothing between the
// router's send and the handler's call is faked, because the receive loop a
// step must not block sits in the middle of it.
type pipelineHop struct {
	router     *ForwardRouter
	agentPeers *node.PeerManager
	handler    *ForwardHandler
	runner     *fakePipelineRunner
}

func newPipelineHop(t *testing.T) *pipelineHop {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	logger := buildLogger()

	addr := loopbackAddress(t)
	t.Setenv("MEMQL_NODE_SERVICE_ADDRESS", addr)
	wbIdentity := &node.Identity{ID: hopWorkbenchId, Type: node.NodeTypeWorkbench, Address: addr}
	runner := newFakePipelineRunner(t)
	handler := pipelineHandler(runner)
	server := node.NewNodeServer(wbIdentity, node.NewPeerManager(wbIdentity, logger), logger)
	server.SetWorkbenchForwardHandler(handler)
	server.Start(ctx)
	t.Cleanup(func() { server.Stop(context.Background()) })
	select {
	case <-server.Ready():
	case <-time.After(5 * time.Second):
		t.Fatalf("the workbench replica's node server never came up on %s", addr)
	}

	agentIdentity := &node.Identity{ID: "agent-a", Type: node.NodeTypeAgent}
	agentPeers := node.NewPeerManager(agentIdentity, logger)
	router := NewForwardRouter(agentPeers, logger)
	dialer := node.NewWorkerDialer(agentIdentity, agentPeers, nil, nil,
		[]node.WorkerTarget{{NodeType: node.NodeTypeWorkbench, Address: addr}}, logger)
	dialer.SetWorkbenchForwardResponseSink(router)
	dialer.Start(ctx)
	t.Cleanup(func() { dialer.Stop(context.Background()) })
	// Registered last, so it runs FIRST: every step is finished before the
	// node server stops, or a step parked on the receive loop would hold the
	// server's graceful stop open and turn a failing test into a hang.
	t.Cleanup(runner.finishAll)

	awaitCondition(t, func() bool { return servingWorkbench(agentPeers) != nil },
		"the agent replica never saw the workbench replica as a healthy, connected peer")
	return &pipelineHop{router: router, agentPeers: agentPeers, handler: handler, runner: runner}
}

// servingWorkbench is the workbench replica as the agent's peer table has it,
// when it is one the router would send to.
func servingWorkbench(peers *node.PeerManager) *node.PeerEntry {
	for _, p := range peers.SnapshotByType(node.NodeTypeWorkbench) {
		if p.Info.GetNodeId() == hopWorkbenchId && healthyWorkbenchPeer(p) {
			return p
		}
	}
	return nil
}

func loopbackAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestForwardCancelReachesARunningPipelineStep. The agent abandons a step (its
// run was cancelled, or a ceiling passed) by ending the forward's context; the
// router sends WorkbenchForwardCancel on the same stream; the workbench's
// receive loop reads it; the handler cancels the step's context, which the
// runner turns into deleting the Job. Every link is real, and the one this pins
// is the receive loop: a handler that ran the step inline would never read the
// cancel, because it is queued behind the step on the loop the step is
// blocking.
func TestForwardCancelReachesARunningPipelineStep(t *testing.T) {
	hop := newPipelineHop(t)

	ctx, abandon := context.WithCancel(context.Background())
	defer abandon()
	stepReq := pipelineForward("cancel-me", PipelineStepAction, `{"stepKey":"test"}`, systemAuthority(t))
	forwarded := make(chan error, 1)
	go func() {
		_, _, err := hop.router.Forward(ctx, stepReq, "")
		forwarded <- err
	}()
	step := hop.runner.awaitStep(t, "the step, across the hop")
	if err := step.ctx.Err(); err != nil {
		t.Fatalf("the step's context was done before anything cancelled it: %v", err)
	}

	abandon()
	select {
	case err := <-forwarded:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Forward = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Forward did not return once its context was cancelled")
	}
	select {
	case <-step.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the step is still running after the forward was abandoned: WorkbenchForwardCancel never reached " +
			"it. A handler that runs a step on the stream's receive loop never reads the cancel queued behind it.")
	}
	awaitCondition(t, func() bool { return trackedRequests(hop.handler) == 0 },
		"the cancelled step is still tracked on the workbench replica")
}

type watchedOutcome struct {
	resp     *nodev1.WorkbenchForwardResponse
	servedBy string
	err      error
}

// TestForwardWatchedReturnsPeerLostWithoutCancel. A step is waited on for
// minutes, and the replica running it can go away in the middle -- a deploy
// drains it, a node dies. Forward would wait out the whole deadline for a reply
// that is never coming. ForwardWatched re-checks the replica it sent to, and
// once that is no longer a replica this node would send to -- unhealthy, or
// disconnected -- ends the wait with ErrWorkbenchPeerLost, naming the replica.
//
// AND IT SENDS NO CANCEL, which is the half that matters. The step's Job runs in
// the cluster, not in the replica: the agent re-forwards and another replica
// adopts the Job by name. A cancel would still reach the handler -- a draining
// replica is still serving its streams -- and the runner would delete the Job.
//
// "No cancel arrived" is a null result, so each case carries the positive that
// gives it meaning: over the SAME stream, a second step's cancel does arrive.
// Messages on one stream arrive in order and the workbench reads them one at a
// time, so by the time the second step has even started, a cancel sent for the
// first would already have ended it.
func TestForwardWatchedReturnsPeerLostWithoutCancel(t *testing.T) {
	cases := []struct {
		name string
		// lose takes the replica away from the agent's view of the mesh while
		// leaving the stream itself up, and returns what puts it back.
		lose func(*pipelineHop) (restore func())
	}{
		{"the replica stops being healthy", func(hop *pipelineHop) func() {
			hop.agentPeers.UpdatePeerHealth(hopWorkbenchId, nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING)
			return func() {
				hop.agentPeers.UpdatePeerHealth(hopWorkbenchId, nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY)
			}
		}},
		{"the replica's connection is detached", func(hop *pipelineHop) func() {
			conn := servingWorkbench(hop.agentPeers).Connection
			hop.agentPeers.DetachConnection(hopWorkbenchId)
			return func() { hop.agentPeers.AttachConnection(hopWorkbenchId, conn) }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hop := newPipelineHop(t)

			waitCtx, stopWaiting := context.WithTimeout(context.Background(), time.Minute)
			defer stopWaiting()
			firstReq := pipelineForward("first", PipelineStepAction, `{"stepKey":"first"}`, systemAuthority(t))
			first := make(chan watchedOutcome, 1)
			go func() {
				resp, servedBy, err := hop.router.ForwardWatched(waitCtx, firstReq, "", 20*time.Millisecond)
				first <- watchedOutcome{resp, servedBy, err}
			}()
			running := hop.runner.awaitStep(t, "the first step, across the hop")
			defer running.finish()

			restore := tc.lose(hop)
			var got watchedOutcome
			select {
			case got = <-first:
			case <-time.After(5 * time.Second):
				t.Fatal("ForwardWatched is still waiting on a replica this node would no longer send to. A step " +
					"whose replica went away waits out its whole deadline for a reply that is never coming.")
			}
			if !errors.Is(got.err, ErrWorkbenchPeerLost) {
				t.Fatalf("ForwardWatched = (%v, %q, %v), want ErrWorkbenchPeerLost", got.resp, got.servedBy, got.err)
			}
			if got.servedBy != hopWorkbenchId {
				t.Fatalf("servedBy = %q, want %q -- the caller re-forwards knowing which replica it lost",
					got.servedBy, hopWorkbenchId)
			}

			// THE POSITIVE, over the same stream.
			restore()
			probeCtx, abandonProbe := context.WithCancel(context.Background())
			defer abandonProbe()
			secondReq := pipelineForward("second", PipelineStepAction, `{"stepKey":"second"}`, systemAuthority(t))
			second := make(chan error, 1)
			go func() {
				_, _, err := hop.router.ForwardWatched(probeCtx, secondReq, "", 20*time.Millisecond)
				second <- err
			}()
			probe := hop.runner.awaitStep(t, "the second step, over the same stream")
			if err := running.ctx.Err(); err != nil {
				t.Fatalf("the first step was cancelled (%v): ForwardWatched sent a cancel when it lost the replica, "+
					"and the runner deletes the Job on a cancel -- the work it exists to keep alive", err)
			}
			abandonProbe()
			select {
			case err := <-second:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("the second ForwardWatched = %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the second ForwardWatched did not return once its context was cancelled")
			}
			select {
			case <-probe.ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the second step's cancel never arrived either, so this stream carries no cancel and the " +
					"first step's survival proves nothing")
			}
			if err := running.ctx.Err(); err != nil {
				t.Fatalf("the first step was cancelled after all (%v)", err)
			}
		})
	}
}

// TestAHungStatusDoesNotHoldTheReceiveLoop (final review, M2): Status, Ack
// and Cancel each read or write the API server, which the runner bounds at
// ten seconds, and an API server that hangs holds each one that long. On the
// peer stream's receive loop -- with every step the agent waits on asked
// after every thirty seconds -- they would keep it blocked: heartbeats, event
// forwards, a running step's cancel and every other forward waiting behind
// them. So each is answered on a goroutine of its own, its reply serialized
// onto the stream with the rest, and the loop reads on.
//
// Over a real stream, because the receive loop is the property: a status the
// runner hangs in, then an ack that must be answered while the status still
// hangs -- and the status, answered once its runner lets go.
func TestAHungStatusDoesNotHoldTheReceiveLoop(t *testing.T) {
	hop := newPipelineHop(t)
	hold, entered := make(chan struct{}), make(chan struct{}, 4)
	var release sync.Once
	// Registered last, so it runs first: a status still held would hold the
	// node server's stop open.
	t.Cleanup(func() { release.Do(func() { close(hold) }) })
	hop.runner.mu.Lock()
	hop.runner.statusHold, hop.runner.statusEntered = hold, entered
	hop.runner.mu.Unlock()

	type answer struct {
		resp *nodev1.WorkbenchForwardResponse
		err  error
	}
	status := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, _, err := hop.router.Forward(ctx,
			pipelineForward("status-hung", PipelineStatusAction, `{"jobName":"mp-1"}`, systemAuthority(t)), "")
		status <- answer{resp, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the status never reached the runner")
	}

	ackCtx, cancelAck := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAck()
	ack, _, err := hop.router.Forward(ackCtx,
		pipelineForward("ack-1", PipelineAckAction, `{"jobName":"mp-1"}`, systemAuthority(t)), "")
	if err != nil {
		t.Fatalf("the ack was not answered while a status hung (%v): the workbench's receive loop is blocked inside "+
			"Status, so nothing else that replica's peer sends is read", err)
	}
	if ack.GetErrorCode() != "" || ack.GetRequestId() != "ack-1" {
		t.Fatalf("the ack's answer = %+v, want the runner's ack, answered", ack)
	}
	select {
	case got := <-status:
		t.Fatalf("the status was answered (%+v, %v) while its runner still held it", got.resp, got.err)
	default:
	}
	if n := trackedRequests(hop.handler); n != 1 {
		t.Errorf("%d requests tracked while the status hangs, want it alone: a cancel must still reach it", n)
	}

	release.Do(func() { close(hold) })
	select {
	case got := <-status:
		if got.err != nil || string(got.resp.GetPayloadJson()) != `{"state":"running"}` {
			t.Fatalf("the released status = %+v, %v; want the runner's reply", got.resp, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the status was never answered once its runner let go")
	}
	awaitCondition(t, func() bool { return trackedRequests(hop.handler) == 0 },
		"a status or an ack is still tracked after it was answered")
}
