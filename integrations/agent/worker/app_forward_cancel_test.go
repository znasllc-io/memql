//go:build agent

package worker

// A CALLER THAT HAS GIVEN UP MUST NOT LEAVE CLAUDE CODE RUNNING ON A LAPTOP,
// and an answer that arrived must not be thrown away.
//
// The realistic path to both: a planner's triage whose first Source is a slow
// local model (fleet qwen took 42s to 2m51s on 2026-09-28) uses up the call's
// deadline, and the structured fallback then reaches app:claude-code with a
// context that is already done. Before these fixes that call still put a
// request on the wire, the cancel behind it was usually lost on the holder,
// and the session ran to its ten-minute ceiling on the owner's subscription.
//
// TO CONFIRM THEY ARE LOAD-BEARING: drop the pre-send context check in
// ForwardAppCall, the holder's caller_cancelled check, the non-blocking read
// in the holder-gone branch, the typed vision rebuild, or the planner's
// delegation-policy read, and the matching test fails.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

func appRequest(content string) memqlengine.AppCallRequest {
	return memqlengine.AppCallRequest{
		ActingUserId: hopOwner,
		AppId:        workerservice.AppIdClaudeCode,
		Messages:     []common.ChatMessage{{Role: "user", Content: content}},
		Schema:       &triageSchema,
	}
}

// AN ALREADY-CANCELLED CALLER SENDS NOTHING. Not a request, and so not the
// cancel that would have had to catch it.
func TestACallerThatAlreadyGaveUpPutsNothingOnTheWire(t *testing.T) {
	h := newAppHop(t)
	ctx, cancel := plannerCtx(t)
	cancel()

	_, err := h.remote.Call(ctx, appRequest("classify: too late"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's own cancellation", err)
	}
	if !errors.Is(err, memqlengine.ErrAppUnavailable) {
		t.Fatalf("a call that did not run must still read as a shut door: %v", err)
	}
	_, requests, cancels := h.link.sent()
	if len(requests) != 0 || len(cancels) != 0 {
		t.Fatalf("a caller that had already given up put %d request(s) and %d cancel(s) on the wire",
			len(requests), len(cancels))
	}
	if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
		t.Fatalf("a caller that had already given up opened %d session(s) on the laptop", len(sessions))
	}
}

// THE ENVELOPE CARRIES THE CALLER'S DEADLINE, not always the ten-minute
// ceiling: a cancel that is lost anyway is then still bounded by what the
// caller was prepared to wait.
func TestTheEnvelopeTimeoutIsBoundedByTheCallersDeadline(t *testing.T) {
	h := newAppHop(t)
	ctx, cancel := plannerCtx(t) // ten seconds
	defer cancel()

	if _, err := h.remote.Call(ctx, appRequest("classify: bounded")); err != nil {
		t.Fatalf("Call: %v", err)
	}
	_, requests, _ := h.link.sent()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if got := requests[0].GetTimeoutSec(); got <= 0 || got > 10 {
		t.Fatalf("the envelope asked the holder for %ds; the caller would wait at most 10s", got)
	}
}

// THE HOLDER REFUSES A CALL WHOSE CALLER HAS GONE, before any session opens.
// The node layer ends the call's context when the cancel arrives; this is the
// check that turns that into "nothing ran".
func TestTheHolderRefusesACallWhoseCallerHasGoneBeforeAnySessionOpens(t *testing.T) {
	h := newAppHop(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var got *nodev1.AppCallForwardResponse
	h.link.handler.HandleForwardedAppCall(ctx, directEnvelope(t, hopOwner, hopOwner, hopMachine),
		func(msg *nodev1.NodeServerMessage) error {
			got = msg.GetAppCallForwardResponse()
			return nil
		})
	if got == nil || got.GetOk() || !got.GetRefusedBeforeStart() || got.GetErrorCode() != ForwardCallerCancelled {
		t.Fatalf("answer = %+v, want a %s refusal before start", got, ForwardCallerCancelled)
	}
	if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
		t.Fatalf("the holder opened %d session(s) for a caller that had gone", len(sessions))
	}

	// And the local door on the holder asks the same question.
	if _, err := h.holder.Call(ctx, appRequest("local, too late")); !errors.Is(err, context.Canceled) {
		t.Fatalf("local call err = %v, want the caller's cancellation", err)
	}
	if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
		t.Fatalf("a local call whose caller had gone opened %d session(s)", len(sessions))
	}
}

// answerThenEndSender is a holder that answers and then drops the stream, both
// before the sender parks -- so the answer and the end are ready at once.
type answerThenEndSender struct{ router *ForwardRouter }

func (s *answerThenEndSender) Send(nodeId string, msg *nodev1.NodeClientMessage) bool {
	_, err := s.SendRequest(nodeId, msg)
	return err == nil
}

func (s *answerThenEndSender) SendRequest(_ string, msg *nodev1.NodeClientMessage) (<-chan struct{}, error) {
	done := make(chan struct{})
	if req := msg.GetAppCallForwardRequest(); req != nil {
		s.router.DispatchAppCall(&nodev1.AppCallForwardResponse{
			RequestId: req.GetRequestId(), Ok: true, Content: "the answer",
		})
	}
	close(done)
	return done, nil
}

// AN ANSWER THAT ARRIVED IS AN ANSWER, even when the stream it came on ended
// straight after: reporting it as app_holder_gone throws Claude Code's work
// away and has the route spend the call a second time on its next Source.
func TestAnAnswerThatArrivedIsNotReportedAsAHolderThatWentAway(t *testing.T) {
	sender := &answerThenEndSender{}
	r := newForwardRouter(sender, func() (string, string) { return hopPlanner, "planner" }, testLogger())
	sender.router = r
	ctx := authorityCtx(t, hopOwner)

	for i := 0; i < 300; i++ {
		out, err := r.ForwardAppCall(ctx, hopHolder, hopMachine, hopOwner, appRequest("race"), time.Minute)
		if err != nil || !out.Ok() || out.Result.Content != "the answer" {
			t.Fatalf("iteration %d: out=%+v err=%v -- an answer that arrived was reported as %q",
				i, out, err, out.ErrorCode)
		}
	}
}

// A VISION-STAGING FAILURE ON THE HOLDER KEEPS ITS TYPE across the hop, and
// stops the walk: "the image did not land" is not "no machine can run this",
// and the owner's next machine on the same holder meets the same storage.
func TestAVisionStagingFailureOnTheHolderKeepsItsTypeAndStopsTheWalk(t *testing.T) {
	h := newAppHop(t)
	// A second laptop, held by the same agent, that could otherwise be tried.
	second := laptopRow()
	second.RegistrationId, second.Name = "laptop-2", "laptop-2"
	h.senderStore.machines = append(h.senderStore.machines, second)
	h.holderStore.machines = append(h.holderStore.machines, second)
	w := &workerservice.Worker{
		RegistrationId: "laptop-2", OwnerUserId: hopOwner, Name: "laptop-2",
		Capabilities: []string{workerservice.CapabilityHeadless},
	}
	w.SetApps([]workerservice.AppInfo{{Id: workerservice.AppIdClaudeCode, Version: "2.1.283", Allowed: true, SignedIn: true}})
	w.SetAppSessionFunc(h.harness.open)
	h.registry.Add(w)

	ctx, cancel := plannerCtx(t)
	defer cancel()
	req := appRequest("what is in this picture")
	req.Schema = nil
	req.Images = []common.VisionContent{{MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}}}

	_, err := h.remote.Call(ctx, req)
	var staging *memqlengine.AppVisionStagingFailed
	if !errors.As(err, &staging) {
		t.Fatalf("err = %v (%T), want the holder's AppVisionStagingFailed -- a storage problem read as "+
			"'no machine can run this app'", err, err)
	}
	if staging.Images != 1 || staging.Reason == "" {
		t.Fatalf("the rebuilt refusal lost what it was about: %+v", staging)
	}
	if _, requests, _ := h.link.sent(); len(requests) != 1 {
		t.Fatalf("the walk tried %d machines; the same holder's storage refuses every one", len(requests))
	}
	if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
		t.Fatalf("%d session(s) opened without the image", len(sessions))
	}
}

// THE PLANNER READS THE OWNER'S APP ORDER. `app:*` must pick the same app on
// the planner as on the agent, or one Route runs Claude Code in triage and
// Codex in the reply for the same owner.
func TestThePlannerWildcardFollowsTheOwnersAppOrder(t *testing.T) {
	h := newAppHop(t)
	both := []workerservice.AppInfo{
		{Id: workerservice.AppIdClaudeCode, Version: "2.1.283", Allowed: true, SignedIn: true},
		{Id: workerservice.AppIdCodex, Version: "0.9", Allowed: true, SignedIn: true},
	}
	h.senderStore.machines[0].Apps = both
	h.holderStore.machines[0].Apps = both
	h.registry.WorkerById(hopMachine).SetApps(both)
	// The engine's own order would take claude-code; the owner put Codex first.
	h.remote.policies = fixedPolicy{policy: DelegationPolicy{Found: true,
		AppOrder: []string{workerservice.AppIdCodex, workerservice.AppIdClaudeCode}}}

	providers := memqlengine.NewProviderRegistryForTest()
	providers.SetAppInference(h.remote)
	ctx, cancel := plannerCtx(t)
	defer cancel()
	entry, _ := providers.EntryForUser(ctx, hopOwner, memqlengine.AppWildcard)
	if !entry.Available {
		t.Fatalf("the planner's wildcard door is shut: %v", entry.Err())
	}
	if _, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(ctx,
		[]common.ChatMessage{{Role: "user", Content: "classify"}}, triageSchema); err != nil {
		t.Fatalf("wildcard call: %v", err)
	}
	_, requests, _ := h.link.sent()
	if len(requests) != 1 || requests[0].GetAppId() != workerservice.AppIdCodex {
		got := ""
		if len(requests) > 0 {
			got = requests[0].GetAppId()
		}
		t.Fatalf("the planner's `app:*` ran %q; the owner's delegation policy puts %q first", got, workerservice.AppIdCodex)
	}
}

// --- every forward's cancel takes the cancel path ---------------------------

// cancelPathSender records which path each envelope took: Send (a request,
// which must fail loudly) or SendCancel (fire-and-forget, queued across a
// reconnect).
type cancelPathSender struct {
	mu      sync.Mutex
	sent    []*nodev1.NodeClientMessage
	cancels []*nodev1.NodeClientMessage
	sentCh  chan struct{}
}

func (s *cancelPathSender) Send(_ string, msg *nodev1.NodeClientMessage) bool {
	s.mu.Lock()
	s.sent = append(s.sent, msg)
	s.mu.Unlock()
	select {
	case s.sentCh <- struct{}{}:
	default:
	}
	return true
}

func (s *cancelPathSender) SendCancel(_ string, msg *nodev1.NodeClientMessage) bool {
	s.mu.Lock()
	s.cancels = append(s.cancels, msg)
	s.mu.Unlock()
	return true
}

func isCancelEnvelope(msg *nodev1.NodeClientMessage) bool {
	switch msg.GetPayload().(type) {
	case *nodev1.NodeClientMessage_WorkerForwardCancel, *nodev1.NodeClientMessage_ModelForwardCancel,
		*nodev1.NodeClientMessage_ModelPullForwardCancel, *nodev1.NodeClientMessage_ModelProbeForwardCancel,
		*nodev1.NodeClientMessage_AppCallForwardCancel:
		return true
	}
	return false
}

func TestEveryForwardSendsItsCancelOnTheCancelPath(t *testing.T) {
	owner := "v1:identity:user:alice"
	cases := []struct {
		name string
		call func(ctx context.Context, r *ForwardRouter)
	}{
		{"tool dispatch", func(ctx context.Context, r *ForwardRouter) {
			req := approvedRequest()
			req.OwnerUserId = owner
			_, _, _ = r.ForwardDispatch(ctx, nodeB, req, "laptop", workerservice.CapabilityHeadless, time.Minute)
		}},
		{"model call", func(ctx context.Context, r *ForwardRouter) {
			_, _ = r.ForwardModelCall(ctx, nodeB, "laptop", owner, &memqlv1.ModelCallStart{Model: "m"}, time.Minute, nil)
		}},
		{"model pull", func(ctx context.Context, r *ForwardRouter) {
			_, _ = r.ForwardModelPull(ctx, nodeB, "laptop", owner, "m", time.Minute, nil)
		}},
		{"model probe", func(ctx context.Context, r *ForwardRouter) {
			_, _ = r.ForwardModelProbe(ctx, nodeB, "laptop", owner, "m", "v1", time.Minute, nil)
		}},
		{"app call", func(ctx context.Context, r *ForwardRouter) {
			_, _ = r.ForwardAppCall(ctx, nodeB, "laptop", owner, appRequest("x"), time.Minute)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &cancelPathSender{sentCh: make(chan struct{}, 1)}
			r := newForwardRouter(sender, func() (string, string) { return nodeA, "agent" }, testLogger())
			ctx, cancel := context.WithCancel(authorityCtx(t, owner))
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call(ctx, r)
			}()
			select {
			case <-sender.sentCh:
			case <-time.After(2 * time.Second):
				t.Fatalf("the %s never sent its request", tc.name)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("the cancelled %s did not return", tc.name)
			}

			sender.mu.Lock()
			defer sender.mu.Unlock()
			for _, m := range sender.sent {
				if isCancelEnvelope(m) {
					t.Fatalf("the %s's cancel went out on the REQUEST path, which drops it during a reconnect", tc.name)
				}
			}
			if len(sender.cancels) != 1 || !isCancelEnvelope(sender.cancels[0]) {
				t.Fatalf("the %s sent %d message(s) on the cancel path, want its one cancel", tc.name, len(sender.cancels))
			}
		})
	}
}

// --- the holder's point of no return, for the other two forwards -------------

func TestAForwardedDispatchWhoseCallerHasGoneNeverReachesTheMachine(t *testing.T) {
	h := newHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Error("a dispatch whose caller had gone reached the machine")
		return nil, nil
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var got *nodev1.WorkerForwardResponse
	h.link.handler.HandleForwardedRequest(ctx, &nodev1.WorkerForwardRequest{
		RequestId: "gone-1", RegistrationId: "laptop", OwnerUserId: h.owner,
		Capability: workerservice.CapabilityHeadless, Tool: "workerHost", Action: "fs_read",
		TimeoutSec: 30, Authority: authorityProtoFor(t, h.owner),
	}, func(m *nodev1.NodeServerMessage) error {
		if r := m.GetWorkerForwardResponse(); r != nil {
			got = r
		}
		return nil
	})
	if got == nil || !got.GetRefusedBeforeStart() || got.GetErrorCode() != ForwardCallerCancelled {
		t.Fatalf("answer = %+v, want a %s refusal before start", got, ForwardCallerCancelled)
	}
}

func TestAForwardedModelCallWhoseCallerHasGoneNeverReachesTheMachine(t *testing.T) {
	h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		t.Error("a model call whose caller had gone reached the machine")
		return workerservice.ModelCallOutcome{}
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startJSON, err := protojson.Marshal(h.start())
	if err != nil {
		t.Fatal(err)
	}

	var got *nodev1.ModelForwardResponse
	h.link.handler.HandleForwardedModelCall(ctx, &nodev1.ModelForwardRequest{
		RequestId: "gone-2", RegistrationId: "laptop", OwnerUserId: h.owner,
		ModelCallStartJson: startJSON, TimeoutSec: 10, Authority: authorityProtoFor(t, h.owner),
	}, func(m *nodev1.NodeServerMessage) error {
		if r := m.GetModelForwardResponse(); r != nil {
			got = r
		}
		return nil
	})
	if got == nil || !got.GetRefusedBeforeStart() || got.GetErrorCode() != ForwardCallerCancelled {
		t.Fatalf("answer = %+v, want a %s refusal before start", got, ForwardCallerCancelled)
	}
}
