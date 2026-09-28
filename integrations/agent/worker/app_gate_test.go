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

// TestAnUnreadableKillSwitchDoesNotCloseTheDoor: the switch closes app sessions
// only when it is explicitly engaged. A preference read that fails is not an
// engaged switch, and neither door treats it as one.
func TestAnUnreadableKillSwitchDoesNotCloseTheDoor(t *testing.T) {
	f := newAppGateFixture(t, "user-1")
	f.store.prefsErr = errors.New("user lookup: statement timeout")

	if _, err := f.executor(t, nil).Run(context.Background(), handedOverStep(), nil); !errors.Is(err, errSessionCaptured) {
		t.Fatalf("session door: err = %v, want the session to reach the machine", err)
	}
	if _, err := f.inference(nil).Call(context.Background(), chatTurn()); !reachedMachine(err) {
		t.Fatalf("chat door: err = %v, want the session to reach the machine", err)
	}
	if got := f.capture.started(); len(got) != 2 {
		t.Fatalf("%d session(s) reached the machine, want 2", len(got))
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
// the chat door: AppSessionStart.workspace EMPTY means the machine chooses. The
// engine names a directory only when the owner set a delegationPolicy
// workspaceRoot, and then it is one directory per session under that root; it
// never invents a path on somebody's machine, and an empty one is sent rather
// than refused.
func TestTheChatDoorLetsTheMachineChooseItsWorkspace(t *testing.T) {
	cases := []struct {
		name     string
		policies DelegationPolicyReader
		wantRoot string
	}{
		{name: "no policy reader", policies: nil},
		{name: "no policy row", policies: fixedPolicy{}},
		{name: "a policy with no root", policies: fixedPolicy{policy: DelegationPolicy{Found: true}}},
		{name: "a policy read that failed", policies: fixedPolicy{err: errors.New("policy read: timeout")}},
		{
			name:     "the owner's root",
			policies: fixedPolicy{policy: DelegationPolicy{Found: true, WorkspaceRoot: "/Users/u/memql-work/"}},
			wantRoot: "/Users/u/memql-work",
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
			want := ""
			if tc.wantRoot != "" {
				want = tc.wantRoot + "/" + lastSegment(got[0].SessionId)
			}
			if got[0].Workspace != want {
				t.Fatalf("workspace = %q, want %q", got[0].Workspace, want)
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
