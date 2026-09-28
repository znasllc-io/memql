//go:build agent

package worker

// THE APP-CALL HOP, tested in process (the planner/app-source design, section
// 3a).
//
// A planner holds no WorkerService stream, and a sibling agent replica holds
// none for this machine, so before AppCallForward a route that put
// `app:claude-code` first passed over it on every planner call -- triage,
// compile, compose -- and on half the agent's. These tests stand up both sides
// for real: the SENDER is the planner's RemoteAppInference behind the real
// provider registry and the real AI router, and the RECEIVER is the holding
// agent's ForwardHandler over its real AppInference, registry and session
// runner. Only the machine's harness is a stand-in (an app-session loopback),
// and the link between the two nodes carries the NodeService envelopes
// serialized through protobuf, so nothing crosses the hop except the wire.
//
// TO CONFIRM IT IS LOAD-BEARING: make RemoteAppInference.Doors report no
// Forwardable machine, or make the receiver skip its registration check or its
// app gate, and these fail. If they pass either way they are worthless.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/router"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

const (
	hopPlanner = "planner-1" // sends: holds no stream at all
	hopHolder  = "agent-2"   // receives: holds the machine's stream
	hopOwner   = "v1:identity:user:alice"
	hopMachine = "laptop"
)

// appLink joins the sending node to the holding agent the way NodeService
// joins two pods. Every envelope is marshalled and unmarshalled, so a field the
// proto does not carry does not arrive.
type appLink struct {
	t       *testing.T
	handler *ForwardHandler
	router  *ForwardRouter

	mu        sync.Mutex
	reachable bool
	targets   []string
	requests  []*nodev1.AppCallForwardRequest
	cancels   []string
	// streamDone, when set, is the stream the request went out on; dropStream
	// closes it and ends the holder's side of the same stream, which is what
	// a holder going away mid-call looks like from both ends.
	streamDone   chan struct{}
	streamCtx    context.Context
	streamCancel context.CancelFunc
	wg           sync.WaitGroup
}

func (l *appLink) dropStream() {
	l.streamCancel()
	close(l.streamDone)
}

func wire[T proto.Message](t *testing.T, in T, out T) T {
	t.Helper()
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func (l *appLink) Send(nodeId string, msg *nodev1.NodeClientMessage) bool {
	l.mu.Lock()
	l.targets = append(l.targets, nodeId)
	reachable := l.reachable
	l.mu.Unlock()
	if !reachable || nodeId != hopHolder {
		return false
	}
	crossed := wire(l.t, msg, &nodev1.NodeClientMessage{})
	switch payload := crossed.GetPayload().(type) {
	case *nodev1.NodeClientMessage_AppCallForwardRequest:
		l.mu.Lock()
		l.requests = append(l.requests, payload.AppCallForwardRequest)
		l.mu.Unlock()
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handler.HandleForwardedAppCall(l.streamCtx, payload.AppCallForwardRequest, l.back)
		}()
	case *nodev1.NodeClientMessage_AppCallForwardCancel:
		l.mu.Lock()
		l.cancels = append(l.cancels, payload.AppCallForwardCancel.GetRequestId())
		l.mu.Unlock()
		l.handler.CancelForwardedAppCall(context.Background(), payload.AppCallForwardCancel.GetRequestId())
	default:
		l.t.Errorf("an app call put %T on the wire", payload)
	}
	return true
}

// SendRequest is the stream-aware transport the production sender has: it
// reports the stream the request left on, so a holder that goes away mid-call
// is noticed rather than waited out.
func (l *appLink) SendRequest(nodeId string, msg *nodev1.NodeClientMessage) (<-chan struct{}, error) {
	if !l.Send(nodeId, msg) {
		return nil, fmt.Errorf("%s is not reachable", nodeId)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.streamDone, nil
}

// back is the holder's `send`: the return leg, over the wire too. Once the
// stream has dropped nothing crosses it any more, which is what makes a lost
// holder a lost ANSWER rather than a late one.
func (l *appLink) back(msg *nodev1.NodeServerMessage) error {
	if l.streamCtx.Err() != nil {
		return fmt.Errorf("the stream to the sender has ended")
	}
	crossed := wire(l.t, msg, &nodev1.NodeServerMessage{})
	if resp := crossed.GetAppCallForwardResponse(); resp != nil {
		l.router.DispatchAppCall(resp)
		return nil
	}
	l.t.Errorf("the holder answered an app call with %T", crossed.GetPayload())
	return nil
}

func (l *appLink) sent() ([]string, []*nodev1.AppCallForwardRequest, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.targets...), append([]*nodev1.AppCallForwardRequest(nil), l.requests...), append([]string(nil), l.cancels...)
}

// machineHarness stands in for Claude Code on the laptop: it records every
// session the holder opened and every control it was sent, and answers the way
// it is told to.
type machineHarness struct {
	mu       sync.Mutex
	starts   []workerservice.AppSessionRequest
	actors   []string
	controls []*memqlv1.AppSessionControl
	// answer is the structured result a session returns; hang leaves the
	// session running until it is cancelled.
	answer string
	hang   bool
}

func (m *machineHarness) open(ctx context.Context, req workerservice.AppSessionRequest) (*workerservice.AppSessionHandle, error) {
	actor := ""
	if access, ok := auth.AccessFromContext(ctx); ok {
		actor = access.UserId
	}
	m.mu.Lock()
	m.starts = append(m.starts, req)
	m.actors = append(m.actors, actor)
	hang, answer := m.hang, m.answer
	m.mu.Unlock()

	var finish func(workerservice.AppSessionOutcome)
	handle, emit, finish := workerservice.NewAppSessionLoopback(req, func(c *memqlv1.AppSessionControl) {
		m.mu.Lock()
		m.controls = append(m.controls, c)
		m.mu.Unlock()
		if c.GetAction() == workerservice.AppSessionActionCancel {
			finish(workerservice.AppSessionOutcome{ExitCode: -1, Error: "cancelled: " + c.GetReason()})
		}
	})
	if !hang {
		go func() {
			emit(workerservice.AppSessionChunk{Stream: workerservice.AppSessionStreamStdout, Data: []byte(answer), Seq: 1})
			finish(workerservice.AppSessionOutcome{
				ExitCode: 0,
				Result:   []byte(answer),
				Model:    "claude-sonnet-5",
				Usage:    workerservice.AppSessionUsage{InputTokens: 120, OutputTokens: 9, Known: true},
			})
		}()
	}
	return handle, nil
}

func (m *machineHarness) sessions() ([]workerservice.AppSessionRequest, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]workerservice.AppSessionRequest(nil), m.starts...), append([]string(nil), m.actors...)
}

func (m *machineHarness) cancelled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.controls {
		if c.GetAction() == workerservice.AppSessionActionCancel {
			return true
		}
	}
	return false
}

// appHop is both nodes and the link between them.
type appHop struct {
	link        *appLink
	harness     *machineHarness
	holderStore *gateStore
	holder      *AppInference
	senderStore *fakeFleet
	remote      *RemoteAppInference
	registry    *workerservice.Registry
}

// laptopRow is the persisted registration: the only thing a planner can read
// about the machine, and what names the replica holding its stream.
func laptopRow() Candidate {
	return machine(hopMachine, func(c *Candidate) {
		c.OwnerUserId = hopOwner
		c.ConnectedNodeId = hopHolder
		c.Apps = []workerservice.AppInfo{{Id: workerservice.AppIdClaudeCode, Version: "2.1.283", Allowed: true, SignedIn: true}}
		c.Labels = map[string]string{workerservice.AppLabelKey(workerservice.AppIdClaudeCode): "2.1"}
	})
}

func newAppHop(t *testing.T) *appHop {
	t.Helper()
	logger := testLogger()

	// THE HOLDING AGENT: a registry with the machine's stream, the machine's
	// app inventory, and the stand-in harness.
	harness := &machineHarness{answer: `{"complexity":"trivial"}`}
	registry := workerservice.NewRegistry(logger, fleetNow)
	w := &workerservice.Worker{
		RegistrationId: hopMachine,
		OwnerUserId:    hopOwner,
		Name:           "laptop",
		Capabilities:   []string{workerservice.CapabilityHeadless},
	}
	w.SetApps([]workerservice.AppInfo{{Id: workerservice.AppIdClaudeCode, Version: "2.1.283", Allowed: true, SignedIn: true}})
	w.SetAppSessionFunc(harness.open)
	registry.Add(w)

	holderStore := &gateStore{fakeFleet: &fakeFleet{machines: []Candidate{laptopRow()}, owner: hopOwner}}
	holder := &AppInference{
		router:   NewRouter(holderStore, logger, fleetNow),
		store:    holderStore,
		registry: registry,
		runner:   &workerservice.SessionRunner{Logger: logger, Minter: fixedMinter{}},
		prefs:    holderStore,
		logger:   logger,
		clock:    fleetNow,
	}
	handler := NewForwardHandler(registry, holderStore, logger)
	handler.SetAppCallServer(holder)

	// THE PLANNER: the persisted row and a forward, and nothing else -- no
	// registry, no session runner, no app gate of its own.
	link := &appLink{t: t, handler: handler, reachable: true}
	link.streamCtx, link.streamCancel = context.WithCancel(context.Background())
	link.router = newForwardRouter(link, func() (string, string) { return hopPlanner, "planner" }, logger)
	senderStore := &fakeFleet{machines: []Candidate{laptopRow()}, owner: hopOwner}
	remote := NewRemoteAppInference(senderStore, link.router, hopPlanner, logger)
	remote.clock = fleetNow

	t.Cleanup(func() {
		link.streamCancel()
		link.wg.Wait()
	})
	return &appHop{link: link, harness: harness, holderStore: holderStore, holder: holder,
		senderStore: senderStore, remote: remote, registry: registry}
}

// plannerCtx is the context a planner's triage runs under: a persisted owner,
// which binds the forwarded authority the hop re-asserts, inside a run whose
// step is named by KEY.
func plannerCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), hopOwner, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "v1:work:run:goal-1", StepKey: "triage", OwnerUserId: hopOwner})
	return context.WithTimeout(ctx, 10*time.Second)
}

// backupSource is the route's second source: a local model that answers.
type backupSource struct {
	mu     sync.Mutex
	answer string
	calls  int
}

func (b *backupSource) reply() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.answer, nil
}
func (b *backupSource) Call(context.Context, string) (any, error) { return b.reply() }
func (b *backupSource) CallChat(context.Context, []common.ChatMessage) (string, error) {
	return b.reply()
}
func (b *backupSource) CallChatStructured(context.Context, []common.ChatMessage, common.StructuredSchema) (string, error) {
	return b.reply()
}
func (b *backupSource) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// plannerRouter is the planner's AI router over the planner's provider
// registry: the RemoteAppInference installed exactly as the planner's cluster
// wiring installs it, and a route that prefers Claude Code with a local model
// behind it -- the 2026-09-28 cluster's fastLocalFirst, in miniature.
func (h *appHop) plannerRouter(t *testing.T) (*router.Router, *memqlengine.ProviderRegistry, *backupSource) {
	t.Helper()
	providers := memqlengine.NewProviderRegistryForTest()
	providers.SetAppInference(h.remote)
	backup := &backupSource{answer: `{"complexity":"from-the-backup"}`}
	providers.RegisterForTest("fleet:backup", "Scripted", "backup", backup)
	rules := memqlengine.NewRuleRegistry()
	if err := rules.Register(&memqlengine.RuleConfig{
		Name: memqlengine.DefaultRuleName, When: memqlengine.RuleWhen{Present: map[string]bool{}},
		Policy: "fastLocalFirst", Locked: true, OnUnavailable: memqlengine.OnUnavailableDegrade,
		SourceFile: "dsl/rules/rules.memql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	policies := memqlengine.NewPolicyRegistryForTest(map[string][]string{
		"fastLocalFirst": {memqlengine.AppReferencePrefix + workerservice.AppIdClaudeCode, "fleet:backup"},
	})
	return router.New(providers, policies, rules, nil, nil), providers, backup
}

var triageSchema = common.StructuredSchema{Name: "goalComplexityTriage", Schema: json.RawMessage(`{"type":"object"}`), Strict: true}

func triageRequest() router.ResolveRequest {
	return router.ResolveRequest{UserId: hopOwner, Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: "goalComplexityTriage"}
}

// triage resolves and calls the planner's structured triage through the router.
func triage(t *testing.T, ctx context.Context, rtr *router.Router, content string) (string, memqlengine.ResolvedProvider, error) {
	t.Helper()
	resolved, err := rtr.ResolveFor(ctx, triageRequest())
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	client, ok := resolved.Client.(common.ChatStructuredProvider)
	if !ok {
		t.Fatalf("the structured client %T does not serve structured output", resolved.Client)
	}
	text, err := client.CallChatStructured(ctx, []common.ChatMessage{{Role: "user", Content: content}}, triageSchema)
	return text, resolved, err
}

// THE ACCEPTANCE: a planner-side structured call whose route prefers
// app:claude-code is served by Claude Code on the laptop, through the agent
// holding the laptop's stream.
func TestAPlannerStructuredCallPreferringClaudeCodeIsServedThroughTheForward(t *testing.T) {
	h := newAppHop(t)
	rtr, providers, backup := h.plannerRouter(t)
	ctx, cancel := plannerCtx(t)
	defer cancel()

	text, resolved, err := triage(t, ctx, rtr, "classify: rename a file")
	if err != nil {
		t.Fatalf("the planner's triage failed: %v", err)
	}
	if resolved.Resolution.ProviderName != memqlengine.AppReferencePrefix+workerservice.AppIdClaudeCode {
		t.Fatalf("the route resolved %q, want the app it prefers -- the planner still passes over app sources",
			resolved.Resolution.ProviderName)
	}
	if text != `{"complexity":"trivial"}` {
		t.Fatalf("answer = %q, want Claude Code's structured result", text)
	}
	if backup.count() != 0 {
		t.Fatalf("the backup source was called %d time(s); the app served the call", backup.count())
	}

	targets, requests, _ := h.link.sent()
	if len(requests) != 1 || len(targets) == 0 || targets[0] != hopHolder {
		t.Fatalf("the call did not cross to the agent holding the stream: targets=%v requests=%d", targets, len(requests))
	}
	// ONE ROUTER, ONE DECISION: what crossed is the resolved call, not a
	// prompt for the holder to route again.
	req := requests[0]
	if req.GetAppId() != workerservice.AppIdClaudeCode || req.GetRegistrationId() != hopMachine || req.GetSchema() == nil {
		t.Fatalf("the envelope does not carry the resolved call: %+v", req)
	}

	sessions, actors := h.harness.sessions()
	if len(sessions) != 1 {
		t.Fatalf("the holder opened %d sessions, want exactly one", len(sessions))
	}
	s := sessions[0]
	if s.App != workerservice.AppIdClaudeCode || s.ResponseSchema == "" || s.Level != string(airoute.LevelFast) {
		t.Fatalf("the session lost the call's shape across the hop: app=%q schema=%q level=%q", s.App, s.ResponseSchema, s.Level)
	}
	if s.RunId != "v1:work:run:goal-1" || s.StepId != "triage" {
		t.Fatalf("the session names run %q step %q, want the planner's run and step key", s.RunId, s.StepId)
	}
	if actors[0] != hopOwner {
		t.Fatalf("the session ran as %q; the planner's authority did not reach the holder", actors[0])
	}

	// The planner's app entry is open for the chat door and shut for a step
	// handover: a tool turn on the planner has no session to open.
	if why := providers.AppSessionRefusalHere(ctx, hopOwner, workerservice.AppIdClaudeCode); why == "" {
		t.Fatal("the planner claims it can open a session for a step handover; it holds no stream")
	}
}

// HOLDER MISSING: the agent the registration names is not reachable from the
// planner. The app door refuses with a named reason before anything started,
// and the route moves on to its next source.
func TestAPlannerCallWhoseHolderIsGoneFallsBackToTheNextSource(t *testing.T) {
	h := newAppHop(t)
	h.link.reachable = false
	rtr, providers, backup := h.plannerRouter(t)
	ctx, cancel := plannerCtx(t)
	defer cancel()

	// The door itself says why it could not serve.
	entry, _ := providers.EntryForUser(ctx, hopOwner, memqlengine.AppReferencePrefix+workerservice.AppIdClaudeCode)
	_, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(ctx,
		[]common.ChatMessage{{Role: "user", Content: "classify: holder gone, direct"}}, triageSchema)
	var refusal *memqlengine.AppUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed app refusal", err)
	}
	if why := refusal.Considered[hopMachine]; !strings.Contains(why, hopHolder) || !strings.Contains(why, "not reachable") {
		t.Fatalf("the refusal does not say the holding agent is unreachable: %q", why)
	}

	// And through the route: the backup serves it.
	text, _, err := triage(t, ctx, rtr, "classify: holder gone, routed")
	if err != nil {
		t.Fatalf("the route did not move on past an unreachable holder: %v", err)
	}
	if text != `{"complexity":"from-the-backup"}` || backup.count() != 1 {
		t.Fatalf("answer=%q backup=%d, want the backup source's answer", text, backup.count())
	}
	if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
		t.Fatalf("a session opened although the holder was unreachable: %d", len(sessions))
	}
}

// A HOLDER THAT NO LONGER HOLDS THE STREAM (the laptop moved to another replica
// since the row was written) refuses before start, and the route moves on.
func TestAHolderThatLostTheStreamRefusesBeforeStartAndTheRouteMovesOn(t *testing.T) {
	h := newAppHop(t)
	h.registry.Remove(hopMachine)
	rtr, _, backup := h.plannerRouter(t)
	ctx, cancel := plannerCtx(t)
	defer cancel()

	text, _, err := triage(t, ctx, rtr, "classify: lost stream")
	if err != nil {
		t.Fatalf("the route did not move on: %v", err)
	}
	if text != `{"complexity":"from-the-backup"}` || backup.count() != 1 {
		t.Fatalf("answer=%q backup=%d, want the backup's", text, backup.count())
	}
}

// directEnvelope is an AppCallForwardRequest built by hand, so a test can say
// exactly what an envelope asserted.
func directEnvelope(t *testing.T, authorityFor, ownerHint, registration string) *nodev1.AppCallForwardRequest {
	t.Helper()
	env := &nodev1.AppCallForwardRequest{
		RequestId:      "direct-1",
		RegistrationId: registration,
		OwnerUserId:    ownerHint,
		AppId:          workerservice.AppIdClaudeCode,
		Messages:       []*nodev1.AppCallMessage{{Role: "user", Content: "hello"}},
		TimeoutSec:     30,
	}
	if authorityFor != "" {
		authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: authorityFor, Role: auth.RoleWriter},
			"", "", time.Time{}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		env.Authority = node.ForwardedAuthorityToProto(authority, hopPlanner, "planner")
	}
	return env
}

// serveDirect hands one envelope to the holder's handler and returns its answer.
func (h *appHop) serveDirect(t *testing.T, env *nodev1.AppCallForwardRequest) *nodev1.AppCallForwardResponse {
	t.Helper()
	var got *nodev1.AppCallForwardResponse
	h.link.handler.HandleForwardedAppCall(context.Background(), env, func(msg *nodev1.NodeServerMessage) error {
		got = msg.GetAppCallForwardResponse()
		return nil
	})
	if got == nil {
		t.Fatal("the holder did not answer the envelope")
	}
	return got
}

// AUTHORITY MISMATCH IS REFUSED, and every refusal says nothing ran.
func TestAForwardedAppCallWithTheWrongAuthorityIsRefused(t *testing.T) {
	cases := []struct {
		name, authorityFor, ownerHint, registration, wantCode string
	}{
		{"no authority at all", "", hopOwner, hopMachine, "forwarded_authority_refused"},
		{"an owner hint the authority does not name", "v1:identity:user:bob", hopOwner, hopMachine, "owner_mismatch"},
		{"somebody else's machine", "v1:identity:user:bob", "v1:identity:user:bob", hopMachine, "registration_refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAppHop(t)
			resp := h.serveDirect(t, directEnvelope(t, tc.authorityFor, tc.ownerHint, tc.registration))
			if resp.GetOk() || resp.GetErrorCode() != tc.wantCode || !resp.GetRefusedBeforeStart() {
				t.Fatalf("answer = %+v, want a %s refusal before start", resp, tc.wantCode)
			}
			if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
				t.Fatalf("a refused envelope opened %d session(s) on the laptop", len(sessions))
			}
		})
	}

	// The positive control: the right authority for the owner's own machine
	// is served, so the refusals above are about the authority and not about
	// an envelope the handler could never serve.
	h := newAppHop(t)
	if resp := h.serveDirect(t, directEnvelope(t, hopOwner, hopOwner, hopMachine)); !resp.GetOk() {
		t.Fatalf("the owner's own envelope was refused: %+v", resp)
	}
}

// THE APP GATE IS THE RECEIVER'S. The kill switch is read on the holder at the
// moment the session would open, and its refusal comes back under its own
// code and type -- the same one a local call gets.
func TestTheHoldersAppGateDecidesAForwardedCall(t *testing.T) {
	h := newAppHop(t)
	h.holderStore.prefs = Preferences{KillSwitchEngaged: true}
	ctx, cancel := plannerCtx(t)
	defer cancel()

	_, err := h.remote.Call(ctx, memqlengine.AppCallRequest{
		ActingUserId: hopOwner, AppId: workerservice.AppIdClaudeCode,
		Messages: []common.ChatMessage{{Role: "user", Content: "gate check"}},
	})
	if code := refusalCode(err); code != AppGateKillSwitchEngaged {
		t.Fatalf("err = %v (code %q), want the holder's %s refusal", err, code, AppGateKillSwitchEngaged)
	}
	if !errors.Is(err, memqlengine.ErrAppUnavailable) {
		t.Fatalf("a gate refusal must read as a shut door so the chain moves on: %v", err)
	}
	if sessions, _ := h.harness.sessions(); len(sessions) != 0 {
		t.Fatal("a session opened past an engaged kill switch")
	}

	// And a pin somebody else made opens nothing on the owner's machine.
	h2 := newAppHop(t)
	_, err = h2.remote.Call(ctx, memqlengine.AppCallRequest{
		ActingUserId: hopOwner, AppId: workerservice.AppIdClaudeCode,
		Messages: []common.ChatMessage{{Role: "user", Content: "pin check"}},
		Pin:      memqlengine.AppDoorPin{Pinned: true, By: "v1:identity:user:mallory"},
	})
	if code := refusalCode(err); code != AppGateNotNamedByOwner {
		t.Fatalf("err = %v (code %q), want %s", err, code, AppGateNotNamedByOwner)
	}
}

// CANCELLATION CROSSES THE HOP. A planner that gives up must not leave Claude
// Code running on somebody's laptop.
func TestACancelledForwardedCallCancelsTheSessionOnTheMachine(t *testing.T) {
	h := newAppHop(t)
	h.harness.hang = true
	ctx, cancel := plannerCtx(t)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := h.remote.Call(ctx, memqlengine.AppCallRequest{
			ActingUserId: hopOwner, AppId: workerservice.AppIdClaudeCode,
			Messages: []common.ChatMessage{{Role: "user", Content: "a long one"}},
		})
		done <- err
	}()
	waitFor(t, "the session to open on the laptop", func() bool {
		sessions, _ := h.harness.sessions()
		return len(sessions) == 1
	})
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the caller's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call did not return")
	}
	waitFor(t, "the cancel to reach the laptop", h.harness.cancelled)
	if _, _, cancels := h.link.sent(); len(cancels) != 1 {
		t.Fatalf("the sender put %d cancel(s) on the wire, want one", len(cancels))
	}
}

// THE HOLDER GOING AWAY MID-CALL IS NOTICED, not waited out. The stream the
// request left on ended, so no answer can come back on it.
func TestAHolderThatGoesAwayMidCallFailsFast(t *testing.T) {
	h := newAppHop(t)
	h.harness.hang = true
	h.link.streamDone = make(chan struct{})
	ctx, cancel := plannerCtx(t)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := h.remote.Call(ctx, memqlengine.AppCallRequest{
			ActingUserId: hopOwner, AppId: workerservice.AppIdClaudeCode,
			Messages: []common.ChatMessage{{Role: "user", Content: "mid-call loss"}},
		})
		done <- err
	}()
	waitFor(t, "the session to open", func() bool {
		sessions, _ := h.harness.sessions()
		return len(sessions) == 1
	})
	h.link.dropStream()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, memqlengine.ErrAppUnavailable) || !strings.Contains(err.Error(), hopHolder) {
			t.Fatalf("err = %v, want a shut door naming the holder that went away", err)
		}
		if ctx.Err() != nil {
			t.Fatal("the call returned only because its own deadline passed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call whose holder went away waited instead of failing")
	}
}

// A SIBLING AGENT REPLICA forwards too: the agent that serves the turn is not
// always the one holding the laptop, and with two replicas that is a coin flip.
func TestAnAgentReplicaWithoutTheStreamForwardsTheCall(t *testing.T) {
	h := newAppHop(t)
	logger := testLogger()
	senderStore := &gateStore{fakeFleet: &fakeFleet{machines: []Candidate{laptopRow()}, owner: hopOwner}}
	sibling := &AppInference{
		router:   NewRouter(senderStore, logger, fleetNow),
		store:    senderStore,
		registry: workerservice.NewRegistry(logger, fleetNow), // holds nothing
		runner:   &workerservice.SessionRunner{Logger: logger, Minter: fixedMinter{}},
		prefs:    senderStore,
		logger:   logger,
		clock:    fleetNow,
	}
	sibling.SetForward(h.link.router, "agent-1")
	ctx, cancel := plannerCtx(t)
	defer cancel()

	doors, err := sibling.Doors(ctx, hopOwner)
	if err != nil {
		t.Fatal(err)
	}
	var door memqlengine.AppDoor
	for _, d := range doors {
		if d.AppId == workerservice.AppIdClaudeCode {
			door = d
		}
	}
	if !door.Reachable() || door.Runnable() {
		t.Fatalf("door = %+v, want reachable by forward and not runnable here", door)
	}

	res, err := sibling.Call(ctx, memqlengine.AppCallRequest{
		ActingUserId: hopOwner, AppId: workerservice.AppIdClaudeCode,
		Messages: []common.ChatMessage{{Role: "user", Content: "from the sibling"}},
		Schema:   &triageSchema,
	})
	if err != nil {
		t.Fatalf("the sibling replica could not reach the laptop: %v", err)
	}
	if res.Content != `{"complexity":"trivial"}` || res.ExecutionSurface != AppSurfacePrefix+workerservice.AppIdClaudeCode+"@"+hopMachine {
		t.Fatalf("result = %+v, want the laptop's answer and its surface", res)
	}
	if res.Model != "claude-sonnet-5" || !res.Usage.Known || res.Usage.InputTokens != 120 || res.Billing == "" {
		t.Fatalf("the app's report did not cross the hop: %+v", res)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
