//go:build agent

package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

// app_inference_recording_test.go -- a model call answered by an app is
// recorded AS a model call.
//
// The chain has three links, and each is pinned where it lives:
//
//   - component/memql fills the calling run and step KEY from the run context
//     (TestAppProviderCallCarriesRunContext);
//   - integrations/work writes a model-call recording that names the calling
//     run and claims nothing on its step -- no childRunId, no inherited goal
//     signature (TestAModelCallRecordingDoesNotClaimTheCallingStep);
//   - and THIS file: the app door's Call marks its session ModelCall, so the
//     recorder is told which of the two kinds of open it is looking at.
//
// The middle link is only as good as the last one. Without the flag the door's
// session opens as a HANDOVER: every chat, structured or vision call inside a
// run stamps childRunId on the calling step -- once per call, each overwriting
// the last -- and joins the Ask goal's procedure corpus as if one classifier
// answer were a recording of the goal's work. Nothing else in the tree fails
// when the flag goes.
//
// TO CONFIRM IT IS LOAD-BEARING: set `ModelCall: false` in AppInference.Call's
// RunSpec, and TestTheAppDoorOpensAModelCallRecording fails.

// openCapture is a SessionRecorder that keeps each RecordingOpen it is handed.
type openCapture struct {
	mu     sync.Mutex
	opened []workerservice.RecordingOpen
}

func (c *openCapture) OpenRecording(_ context.Context, r workerservice.RecordingOpen) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened = append(c.opened, r)
	return "v1:work:run:recording", nil
}

func (c *openCapture) RecordAction(context.Context, workerservice.RecordedAction) error { return nil }
func (c *openCapture) RecordGap(context.Context, workerservice.RecordedGap) error       { return nil }
func (c *openCapture) CloseRecording(context.Context, workerservice.RecordingClose) error {
	return nil
}
func (c *openCapture) HeartbeatRecording(context.Context, workerservice.RecordingHeartbeat) error {
	return nil
}

func (c *openCapture) opens() []workerservice.RecordingOpen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]workerservice.RecordingOpen(nil), c.opened...)
}

// stubMinter hands out a bearer without an identity service.
type stubMinter struct{}

func (stubMinter) Mint(_ context.Context, req workerservice.CredentialRequest) (workerservice.Credential, error) {
	return workerservice.Credential{
		Token:      "app-session-bearer",
		ExpiresAt:  time.Now().Add(req.TTL),
		IdentityId: "app-session:" + req.SessionId,
	}, nil
}

// appDoorFixture is one signed-in Claude Code on a machine THIS replica holds,
// behind the real provider registry, app door and session runner. The machine
// refuses every start: what is under test is the session the door OPENS, and
// the recording is opened before the start goes on the wire -- so a refused
// start measures it without a session loopback this package cannot build.
func appDoorFixture(t *testing.T, owner string) (*memqlengine.ProviderRegistry, *openCapture, <-chan workerservice.AppSessionRequest) {
	t.Helper()
	logger := slog.Default()
	registry := workerservice.NewRegistry(logger, nil)

	started := make(chan workerservice.AppSessionRequest, 4)
	apps := []workerservice.AppInfo{
		{Id: workerservice.AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true},
	}
	w := &workerservice.Worker{
		RegistrationId: "reg-app",
		OwnerUserId:    owner,
		Capabilities:   []string{workerservice.CapabilityHeadless},
	}
	w.SetApps(apps)
	w.SetAppSessionFunc(func(_ context.Context, req workerservice.AppSessionRequest) (*workerservice.AppSessionHandle, error) {
		started <- req
		return nil, errors.New("the machine went to sleep before the session started")
	})
	registry.Add(w)

	store := fakeFleetStore{candidates: []Candidate{{
		RegistrationId:  "reg-app",
		OwnerUserId:     owner,
		ConnectedNodeId: "agent-test",
		// Fresh, or the router drops the machine as offline before it looks
		// at anything else and the test passes for the wrong reason.
		LastSeenAt:   time.Now().UTC(),
		Capabilities: []string{workerservice.CapabilityHeadless},
		Labels:       map[string]string{workerservice.AppLabelKey(workerservice.AppIdClaudeCode): "2.1"},
		Apps:         apps,
	}}}

	recorder := &openCapture{}
	door := &AppInference{
		router:   NewRouter(store, logger, nil),
		store:    store,
		registry: registry,
		runner:   &workerservice.SessionRunner{Logger: logger, Minter: stubMinter{}, Recorder: recorder},
		logger:   logger,
		clock:    func() time.Time { return time.Now().UTC() },
	}
	providers := memqlengine.NewProviderRegistryForTest()
	providers.SetAppInference(door)
	return providers, recorder, started
}

// TestTheAppDoorOpensAModelCallRecording drives a chat call through the whole
// door -- the provider's run-context fill, the app door's Call, the session
// runner -- from inside a run, and reads what the recorder was told.
func TestTheAppDoorOpensAModelCallRecording(t *testing.T) {
	const owner = "v1:identity:user:alice"
	providers, recorder, started := appDoorFixture(t, owner)

	entry, ok := providers.EntryForUser(context.Background(), owner, memqlengine.AppReferencePrefix+workerservice.AppIdClaudeCode)
	if !ok || entry == nil || !entry.Available {
		t.Fatalf("the fixture's door is not open (entry %+v); every assertion below would pass for the wrong reason", entry)
	}
	chat, ok := entry.Client.(common.ChatAIProvider)
	if !ok {
		t.Fatalf("the app door's client %T does not serve chat", entry.Client)
	}

	inRun := common.ContextWithRun(context.Background(), common.RunContext{RunId: "v1:work:run:ask", StepKey: "reason"})
	if _, err := chat.CallChat(inRun, []common.ChatMessage{{Role: "user", Content: "classify this failure"}}); err == nil {
		t.Fatal("the machine refused the start, and the call reported an answer")
	}

	opens := recorder.opens()
	if len(opens) != 1 {
		t.Fatalf("the door's session opened %d recordings, want 1", len(opens))
	}
	opened := opens[0]
	if !opened.ModelCall {
		t.Error("the app door's session opened as a HANDOVER: its recording would stamp childRunId on the " +
			"calling step, once per call, and join the goal's procedure corpus -- it must be marked ModelCall")
	}
	if opened.ParentRunId != "v1:work:run:ask" || opened.ParentStepId != "reason" {
		t.Errorf("the recording names parent run %q step %q, want the calling run and its step KEY -- "+
			"a model call's session has to be traceable to the work that caused it",
			opened.ParentRunId, opened.ParentStepId)
	}
	if opened.RunId != "" {
		t.Errorf("the door supplied recording run %q; only a handover's driver opens the run for the recorder", opened.RunId)
	}

	select {
	case req := <-started:
		if req.RunId != "v1:work:run:ask" || req.StepId != "reason" {
			t.Errorf("AppSessionStart names run %q step %q, want the calling run and step key", req.RunId, req.StepId)
		}
	default:
		t.Fatal("the door never asked the machine to start the session")
	}
}

// TestTheAppDoorOutsideARunIsStillAModelCall is the control: with no run
// there is no parent to name, and the session is still one model call rather
// than a step handed over.
func TestTheAppDoorOutsideARunIsStillAModelCall(t *testing.T) {
	const owner = "v1:identity:user:alice"
	providers, recorder, _ := appDoorFixture(t, owner)

	entry, ok := providers.EntryForUser(context.Background(), owner, memqlengine.AppReferencePrefix+workerservice.AppIdClaudeCode)
	if !ok || entry == nil || !entry.Available {
		t.Fatalf("the fixture's door is not open (entry %+v)", entry)
	}
	if _, err := entry.Client.(common.ChatAIProvider).CallChat(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "hello"}}); err == nil {
		t.Fatal("the machine refused the start, and the call reported an answer")
	}
	opens := recorder.opens()
	if len(opens) != 1 {
		t.Fatalf("the door's session opened %d recordings, want 1", len(opens))
	}
	if !opens[0].ModelCall || opens[0].ParentRunId != "" || opens[0].ParentStepId != "" {
		t.Errorf("outside a run the recording opened as %+v, want a model call naming no parent", opens[0])
	}
}
