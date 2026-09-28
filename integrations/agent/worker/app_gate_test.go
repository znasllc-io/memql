//go:build agent

package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/planner"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/common"
)

// The app gate's tests drive BOTH doors to the machine's edge: a real Router,
// a real Registry and a real SessionRunner, with only the stream replaced by a
// hook that records what would have gone on the wire. "Admitted" therefore
// means what it means in production -- the session reached the machine --
// rather than "no gate returned an error", which a gate that never ran would
// also satisfy.

// errSessionCaptured is what the recording hook answers instead of opening a
// session. The runner treats it as a start failure, which is the cheapest way
// out of Run once the request has been captured.
var errSessionCaptured = errors.New("session captured by the test")

// sessionCapture is the machine's app-session hook in these tests.
type sessionCapture struct {
	mu   sync.Mutex
	reqs []workerservice.AppSessionRequest
}

func (c *sessionCapture) open(_ context.Context, req workerservice.AppSessionRequest) (*workerservice.AppSessionHandle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return nil, errSessionCaptured
}

func (c *sessionCapture) started() []workerservice.AppSessionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]workerservice.AppSessionRequest(nil), c.reqs...)
}

// reachedMachine reports whether a door's error is the hook's own answer. The
// chat door reports a start failure as text under ErrAppUnavailable rather
// than wrapping it, so the hook's error is recognised by its message there.
func reachedMachine(err error) bool {
	return err != nil && strings.Contains(err.Error(), errSessionCaptured.Error())
}

// gateStore is the dispatcher's Store with the gate reads scripted. It has NO
// agentAuthorization row for anybody, which is the cluster the app door has to
// work on: nobody has granted an agent a shell, and nobody needs to for an app
// session. authorizationReads counts the reads so a test can say the gate never
// asked.
type gateStore struct {
	*fakeFleet
	prefs              Preferences
	prefsErr           error
	authorizationReads int
}

func (s *gateStore) UserPreferences(context.Context, string) (Preferences, error) {
	return s.prefs, s.prefsErr
}

func (s *gateStore) AgentAuthorization(context.Context, string, string) (*Authorization, error) {
	s.authorizationReads++
	return nil, nil
}

func (s *gateStore) WriteInvocation(context.Context, workerservice.InvocationRow) error { return nil }

// fixedMinter hands out a bearer without an identity service.
type fixedMinter struct{}

func (fixedMinter) Mint(_ context.Context, req workerservice.CredentialRequest) (workerservice.Credential, error) {
	return workerservice.Credential{
		Token:      "bearer-for-" + req.SessionId,
		ExpiresAt:  time.Now().Add(req.TTL),
		IdentityId: "v1:identity:identity:session-cred",
	}, nil
}

// fixedPolicy is a DelegationPolicyReader with one answer.
type fixedPolicy struct {
	policy DelegationPolicy
	err    error
}

func (p fixedPolicy) DelegationPolicy(context.Context, string) (DelegationPolicy, error) {
	return p.policy, p.err
}

// appGateFixture is one owner, user-1, with one machine online whose stream
// THIS replica holds and on which claude-code is allowed and signed in. The
// machine's live registration is owned by liveOwner, which is user-1 unless a
// test is about a registration that is not.
type appGateFixture struct {
	store      *gateStore
	dispatcher *Dispatcher
	runner     *workerservice.SessionRunner
	machine    *workerservice.Worker
	capture    *sessionCapture
}

func newAppGateFixture(t *testing.T, liveOwner string) *appGateFixture {
	t.Helper()
	logger := testLogger()
	registry := workerservice.NewRegistry(logger, nil)

	capture := &sessionCapture{}
	w := &workerservice.Worker{
		RegistrationId: "reg-app",
		OwnerUserId:    liveOwner,
		Name:           "laptop",
		Capabilities:   []string{workerservice.CapabilityHeadless},
	}
	w.SetApps([]workerservice.AppInfo{
		{Id: workerservice.AppIdClaudeCode, Version: "2.1.283", Allowed: true, SignedIn: true},
	})
	w.SetAppSessionFunc(capture.open)
	registry.Add(w)

	store := &gateStore{fakeFleet: &fakeFleet{machines: []Candidate{
		machine("reg-app", withLabels(map[string]string{
			workerservice.AppLabelKey(workerservice.AppIdClaudeCode): "2.1",
		})),
	}}}
	dispatcher, err := NewDispatcher(Options{
		Logger:   logger,
		Registry: registry,
		Store:    store,
		Clock:    fleetNow,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return &appGateFixture{
		store:      store,
		dispatcher: dispatcher,
		runner:     &workerservice.SessionRunner{Logger: logger, Minter: fixedMinter{}},
		machine:    w,
		capture:    capture,
	}
}

func (f *appGateFixture) executor(t *testing.T, policies DelegationPolicyReader) *CockpitAppExecutor {
	t.Helper()
	exec, err := NewCockpitAppExecutor(testLogger(), f.dispatcher, f.runner, policies)
	if err != nil {
		t.Fatalf("NewCockpitAppExecutor: %v", err)
	}
	return exec
}

func (f *appGateFixture) inference(policies DelegationPolicyReader) *AppInference {
	return NewAppInference(f.dispatcher, f.runner, policies, testLogger())
}

// journalWrites counts what a delegate's journal wrote. A child run is the
// first thing the journal writes, so zero writes is "no child run opened".
type journalWrites struct {
	mu      sync.Mutex
	queries []string
}

func (j *journalWrites) journal() *workjournal.Journal {
	return workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.queries = append(j.queries, q)
		return nil, nil
	}), nil, "node-a")
}

func (j *journalWrites) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.queries)
}

// delegate is the session door's own entry point over the real executor, with
// a journal whose writes are counted.
func (f *appGateFixture) delegate(t *testing.T, writes *journalWrites) *AppSessionDelegate {
	t.Helper()
	return NewAppSessionDelegate(f.executor(t, nil), writes.journal(), nil, testLogger())
}

// refusalCode is the app gate's named code on err, or "" when err is not a
// refusal from the gate.
func refusalCode(err error) string {
	var refusal *appGateRefusal
	if errors.As(err, &refusal) {
		return refusal.Code()
	}
	return ""
}

// sessionHandover is what the router's session door hands the delegate for an
// Ask's `reason` step, reached through pin.
func sessionHandover(pin memqlengine.AppDoorPin) memqlengine.AppSessionHandover {
	return memqlengine.AppSessionHandover{
		ActingUserId: "user-1",
		AgentId:      "v1:agents:agent:assistant",
		AppId:        workerservice.AppIdClaudeCode,
		RunId:        "v1:work:run:ask-1",
		StepId:       "reason",
		Prompt:       "hello",
		Pin:          pin,
	}
}

func pinnedChatTurn(pin memqlengine.AppDoorPin) memqlengine.AppCallRequest {
	req := chatTurn()
	req.Pin = pin
	return req
}

// handedOverStep is what the session door (design D7) hands the executor for
// an Ask's `reason` step.
func handedOverStep() planner.ExecutorRequest {
	return planner.ExecutorRequest{
		OwnerUserId: "user-1",
		AgentId:     "v1:agents:agent:assistant",
		RunId:       "v1:work:run:ask-1",
		StepId:      "reason",
		Kind:        appSessionTemplate,
		Input: map[string]any{
			"executorBackend": BackendCockpitApp + ":" + workerservice.AppIdClaudeCode,
			"prompt":          "hello",
		},
	}
}

func chatTurn() memqlengine.AppCallRequest {
	return memqlengine.AppCallRequest{
		ActingUserId: "user-1",
		AppId:        workerservice.AppIdClaudeCode,
		Messages:     []common.ChatMessage{{Role: "user", Content: "hello"}},
	}
}

// TestAnAppSessionOpensWithNoAgentAuthorization is the owner's decision on
// app-session consent: the gate is the machine's apps.allow plus the owner's
// policy naming the app, and NOT the computer-use scope. A cluster where no
// agent was ever granted a shell -- zero agentAuthorization rows, the kill
// switch never touched -- still opens the session a policy routed to an
// allowed, signed-in app, and the gate never reads an agent's scope to decide.
func TestAnAppSessionOpensWithNoAgentAuthorization(t *testing.T) {
	f := newAppGateFixture(t, "user-1")
	// The kill switch never touched: the zero value IS the unset switch.
	f.store.prefs = Preferences{}

	_, err := f.executor(t, nil).Run(context.Background(), handedOverStep(), nil)

	if got := f.capture.started(); len(got) != 1 {
		t.Fatalf("the session never reached the machine (err: %v); an allowed, signed-in app on the "+
			"owner's machine must be admitted without an agentAuthorization row", err)
	}
	if !errors.Is(err, errSessionCaptured) {
		t.Fatalf("the run ended on %v, want the test hook's own answer -- anything else is a gate "+
			"refusing after the session was already started", err)
	}
	if f.store.authorizationReads != 0 {
		t.Fatalf("the app gate read agentAuthorization %d time(s); an app session's consent is not an "+
			"agent's computer-use scope", f.store.authorizationReads)
	}
}

// TestTheExplicitKillSwitchClosesBothDoorsByName: the one MemQL-side switch
// the owner can throw does close app sessions, on the session door and the
// chat door alike, and says so by its own name -- before any machine is
// selected, so nothing is opened on the machine.
func TestTheExplicitKillSwitchClosesBothDoorsByName(t *testing.T) {
	f := newAppGateFixture(t, "user-1")
	f.store.prefs = Preferences{KillSwitchEngaged: true}

	_, err := f.executor(t, nil).Run(context.Background(), handedOverStep(), nil)
	if err == nil || !strings.Contains(err.Error(), "kill_switch_engaged") {
		t.Fatalf("session door: err = %v, want the kill switch named", err)
	}

	_, err = f.inference(nil).Call(context.Background(), chatTurn())
	if err == nil || !strings.Contains(err.Error(), "kill_switch_engaged") {
		t.Fatalf("chat door: err = %v, want the kill switch named", err)
	}
	if !errors.Is(err, memqlengine.ErrAppUnavailable) {
		t.Fatalf("chat door: err = %v, want it to read as a shut door so a chat chain moves on", err)
	}

	if got := f.capture.started(); len(got) != 0 {
		t.Fatalf("%d session(s) reached the machine with the kill switch engaged", len(got))
	}
}

// TestAnUnreadableKillSwitchRefusesByItsOwnName: a preference read that FAILS
// says nothing about what the owner did, so it neither admits (the switch may
// be engaged, and a database hiccup must not open a session on a machine the
// owner switched off) nor reads as the switch (the owner did not switch
// anything off, and the refusal must not tell them they did). Both doors
// refuse under the read's own code, before a machine is chosen.
func TestAnUnreadableKillSwitchRefusesByItsOwnName(t *testing.T) {
	f := newAppGateFixture(t, "user-1")
	f.store.prefsErr = errors.New("user lookup: statement timeout")

	_, err := f.executor(t, nil).Run(context.Background(), handedOverStep(), nil)
	if code := refusalCode(err); code != AppGateKillSwitchUnreadable {
		t.Fatalf("session door: refusal %q (err: %v), want %q", code, err, AppGateKillSwitchUnreadable)
	}

	_, err = f.inference(nil).Call(context.Background(), chatTurn())
	if code := refusalCode(err); code != AppGateKillSwitchUnreadable {
		t.Fatalf("chat door: refusal %q (err: %v), want %q", code, err, AppGateKillSwitchUnreadable)
	}
	if !errors.Is(err, memqlengine.ErrAppUnavailable) {
		t.Fatalf("chat door: err = %v, want it to read as a shut door so a chat chain moves on", err)
	}
	if strings.Contains(err.Error(), AppGateKillSwitchEngaged) {
		t.Fatalf("chat door: err = %v names the switch as engaged, and nobody engaged it", err)
	}
	if got := f.capture.started(); len(got) != 0 {
		t.Fatalf("%d session(s) reached the machine with the switch unreadable", len(got))
	}
}

// TestOnlyAnExplicitFalseEngagesTheKillSwitch pins the store's read of
// preferences.computerUseEnabled. The switch is something a person throws, so
// only an explicit false engages it; a user who never opened the setting --
// no preferences object, no key -- has not switched anything off.
func TestOnlyAnExplicitFalseEngagesTheKillSwitch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prefs map[string]any
		want  bool
	}{
		{name: "no preferences object", prefs: nil, want: false},
		{name: "no key", prefs: map[string]any{"theme": "dark"}, want: false},
		{name: "switched on", prefs: map[string]any{"computerUseEnabled": true}, want: false},
		{name: "not a boolean", prefs: map[string]any{"computerUseEnabled": "false"}, want: false},
		{name: "explicitly switched off", prefs: map[string]any{"computerUseEnabled": false}, want: true},
	} {
		if got := killSwitchEngaged(tc.prefs); got != tc.want {
			t.Errorf("%s: engaged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAnAppSessionRunsOnlyOnItsOwnersMachine: the registration a session opens
// on must be the owner's. App sessions have no sharing opt-in -- the session's
// back-channel credential names a person, and it is the owner -- so a live
// registration held by somebody else is refused on both doors, by name, even
// when a fleet read returned it for this owner.
func TestAnAppSessionRunsOnlyOnItsOwnersMachine(t *testing.T) {
	f := newAppGateFixture(t, "user-2")

	_, err := f.executor(t, nil).Run(context.Background(), handedOverStep(), nil)
	if err == nil || !strings.Contains(err.Error(), "not user-1's machine") {
		t.Fatalf("session door: err = %v, want the ownership refusal named", err)
	}
	_, err = f.inference(nil).Call(context.Background(), chatTurn())
	if err == nil || !strings.Contains(err.Error(), "not user-1's machine") {
		t.Fatalf("chat door: err = %v, want the ownership refusal named", err)
	}
	if got := f.capture.started(); len(got) != 0 {
		t.Fatalf("%d session(s) opened on another user's machine", len(got))
	}
}

// TestTheChatDoorLetsTheMachineChooseItsWorkspace is the workspace contract on
// the chat door: AppSessionStart.workspace EMPTY means the machine chooses, and
// an inference turn ALWAYS leaves the choice to the machine -- the owner's
// delegationPolicy.workspaceRoot included. A turn is one session and nothing
// outlives it, so a directory the engine named for it would be a directory
// nobody removes (the machine cannot tell it from a run's, which later steps
// reuse), holding copies of every image the turn was shown. The machine's
// scratch directory is created and removed with the session. The engine never
// invents a path on somebody's machine, and an empty one is sent rather than
// refused.
func TestTheChatDoorLetsTheMachineChooseItsWorkspace(t *testing.T) {
	cases := []struct {
		name     string
		policies DelegationPolicyReader
	}{
		{name: "no policy reader", policies: nil},
		{name: "no policy row", policies: fixedPolicy{}},
		{name: "a policy with no root", policies: fixedPolicy{policy: DelegationPolicy{Found: true}}},
		{name: "a policy read that failed", policies: fixedPolicy{err: errors.New("policy read: timeout")}},
		{
			name:     "the owner's root",
			policies: fixedPolicy{policy: DelegationPolicy{Found: true, WorkspaceRoot: "/Users/u/memql-work/"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppGateFixture(t, "user-1")

			if _, err := f.inference(tc.policies).Call(context.Background(), chatTurn()); !reachedMachine(err) {
				t.Fatalf("err = %v, want the session to reach the machine", err)
			}
			got := f.capture.started()
			if len(got) != 1 {
				t.Fatalf("%d session(s) reached the machine, want 1", len(got))
			}
			if got[0].Workspace != "" {
				t.Fatalf("workspace = %q, want empty (the machine chooses)", got[0].Workspace)
			}
		})
	}
}

// TestTheSessionDoorLetsTheMachineChooseWithoutARoot: the handover path keeps
// its one-directory-per-run convention under the owner's root, and with no
// root sends an empty workspace for the machine to choose -- the session is
// still opened, never refused for it.
func TestTheSessionDoorLetsTheMachineChooseWithoutARoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policies DelegationPolicyReader
		want     string
	}{
		{name: "no root", policies: fixedPolicy{}, want: ""},
		{
			name:     "the owner's root",
			policies: fixedPolicy{policy: DelegationPolicy{Found: true, WorkspaceRoot: "/Users/u/memql-work"}},
			want:     "/Users/u/memql-work/v1:work:run:ask-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppGateFixture(t, "user-1")

			if _, err := f.executor(t, tc.policies).Run(context.Background(), handedOverStep(), nil); !errors.Is(err, errSessionCaptured) {
				t.Fatalf("err = %v, want the session to reach the machine", err)
			}
			got := f.capture.started()
			if len(got) != 1 || got[0].Workspace != tc.want {
				t.Fatalf("sessions = %+v, want one with workspace %q", got, tc.want)
			}
		})
	}
}

// TestAWorkspaceWithoutAUnitOfWorkIsTheMachines: a root and nothing to key a
// directory by -- no run, no fresh directory -- names no workspace rather than
// the root itself. Running a session in the root would put it in the tree
// every other run's directory lives in.
func TestAWorkspaceWithoutAUnitOfWorkIsTheMachines(t *testing.T) {
	if got := sessionWorkspace(planner.ExecutorRequest{}, "/Users/u/memql-work"); got != "" {
		t.Fatalf("workspace = %q, want empty (the machine chooses)", got)
	}
}

// TestAPinTheOwnerDidNotMakeOpensNoSession is the other half of the owner's
// consent. A routing rule's chain names an app only because the routing
// configuration wrote it there; an EXPLICIT PIN skips every rule, so it is
// consent only when the person the session runs for made it. Here an agent
// owned by user-2 stores the pin app:claude-code and a turn of user-1's run
// resolves through it: the session would open on user-1's machine, edit files
// and run commands there, and call MemQL with user-1's credential, for a
// choice user-1 never made. Both doors refuse it by name, before a machine is
// chosen -- and the session door before a child run is opened. A pin that no
// person made (a prompt author's default, a deploy env var) is refused the
// same way.
func TestAPinTheOwnerDidNotMakeOpensNoSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  memqlengine.AppDoorPin
	}{
		{name: "another user's agent pinned it", pin: memqlengine.AppDoorPin{Pinned: true, By: "v1:identity:user:user-2"}},
		{name: "no person pinned it", pin: memqlengine.AppDoorPin{Pinned: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppGateFixture(t, "user-1")
			writes := &journalWrites{}

			_, err := f.delegate(t, writes).RunStep(context.Background(), sessionHandover(tc.pin))
			if code := refusalCode(err); code != AppGateNotNamedByOwner {
				t.Fatalf("session door: refusal %q (err: %v), want %q", code, err, AppGateNotNamedByOwner)
			}
			if n := writes.count(); n != 0 {
				t.Fatalf("session door: the refused step still wrote %d journal row(s); a refusal opens no child run", n)
			}

			_, err = f.inference(nil).Call(context.Background(), pinnedChatTurn(tc.pin))
			if code := refusalCode(err); code != AppGateNotNamedByOwner {
				t.Fatalf("chat door: refusal %q (err: %v), want %q", code, err, AppGateNotNamedByOwner)
			}
			if !errors.Is(err, memqlengine.ErrAppUnavailable) {
				t.Fatalf("chat door: err = %v, want it to read as a shut door", err)
			}

			if got := f.capture.started(); len(got) != 0 {
				t.Fatalf("%d session(s) reached user-1's machine through a pin user-1 did not make", len(got))
			}
		})
	}
}

// TestTheOwnersOwnPinAndARulesChainBothOpenTheSession is the control: a pin the
// session's owner made -- their own step override, their own request naming
// the app, whichever spelling of their id it carries -- and a chain a routing
// rule chose both open the session on both doors.
func TestTheOwnersOwnPinAndARulesChainBothOpenTheSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  memqlengine.AppDoorPin
	}{
		{name: "a rule's chain", pin: memqlengine.AppDoorPin{}},
		{name: "the owner's own pin", pin: memqlengine.AppDoorPin{Pinned: true, By: "user-1"}},
		{name: "the owner's own pin, canonical", pin: memqlengine.AppDoorPin{Pinned: true, By: "v1:identity:user:user-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppGateFixture(t, "user-1")
			writes := &journalWrites{}

			if _, err := f.delegate(t, writes).RunStep(context.Background(), sessionHandover(tc.pin)); !errors.Is(err, errSessionCaptured) {
				t.Fatalf("session door: err = %v, want the session to reach the machine", err)
			}
			if _, err := f.inference(nil).Call(context.Background(), pinnedChatTurn(tc.pin)); !reachedMachine(err) {
				t.Fatalf("chat door: err = %v, want the session to reach the machine", err)
			}
			if got := f.capture.started(); len(got) != 2 {
				t.Fatalf("%d session(s) reached the machine, want 2", len(got))
			}
		})
	}
}

// TestARefusedSessionDoorOpensNoChildRun: the gate is asked before the session
// door opens anything. A refused step leaves no child v1:work:run behind, no
// childRunId on the parent step, and its refusal under its own code -- not a
// child run failed as app_session_failed with the reason buried in its text.
func TestARefusedSessionDoorOpensNoChildRun(t *testing.T) {
	f := newAppGateFixture(t, "user-1")
	f.store.prefs = Preferences{KillSwitchEngaged: true}
	writes := &journalWrites{}

	out, err := f.delegate(t, writes).RunStep(context.Background(), sessionHandover(memqlengine.AppDoorPin{}))
	if code := refusalCode(err); code != AppGateKillSwitchEngaged {
		t.Fatalf("refusal %q (err: %v), want %q", code, err, AppGateKillSwitchEngaged)
	}
	if out.ChildRunId != "" || writes.count() != 0 {
		t.Fatalf("the refused step opened child run %q with %d journal write(s)", out.ChildRunId, writes.count())
	}
	if got := f.capture.started(); len(got) != 0 {
		t.Fatalf("%d session(s) reached the machine with the kill switch engaged", len(got))
	}
}
