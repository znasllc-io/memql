package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestOwnerFeaturesPreserveExistingAccessAndRejectMalformedChoices(t *testing.T) {
	if got := OwnerFeaturesFromRow(nil); !got.Headless || !got.Apps || !got.ComputerUse {
		t.Fatal(got)
	}
	got := OwnerFeaturesFromRow(map[string]any{"headless": false, "apps": "true", "computerUse": true})
	if got.Headless || got.Apps || !got.ComputerUse {
		t.Fatal(got)
	}
	if got := OwnerFeaturesFromRow("invalid"); got.Headless || got.Apps || got.ComputerUse {
		t.Fatal(got)
	}
}

type featureTestStore struct {
	*fakeRegistrationStore
	policy      atomic.Pointer[OwnerFeaturePolicy]
	unavailable atomic.Bool
}

func (s *featureTestStore) ReadOwnerFeaturePolicy(context.Context, string, string) (OwnerFeaturePolicy, error) {
	if s.unavailable.Load() {
		return OwnerFeaturePolicy{}, errors.New("settings unavailable")
	}
	return *s.policy.Load(), nil
}

func TestFeatureDisableFromAnotherReplicaCancelsOnlyAffectedWorkAndDoesNotReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, wire := newToolStreamTestSession()
		defer s.cancel()
		store := &featureTestStore{fakeRegistrationStore: &fakeRegistrationStore{}}
		store.policy.Store(&OwnerFeaturePolicy{Features: OwnerFeaturesFromRow(nil), Version: time.Unix(1, 0)})
		s.server.store = store
		s.server.registry.Add(s.worker)
		watcher := NewRegistrationWatcher(s.server.registry, s.server.nodeId, nil)
		done := make(chan error, 1)
		go func() {
			_, err := s.dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "one", Tool: "workerHost", Action: "exec"}, nil)
			done <- err
		}()
		app, err := s.openAppSession(context.Background(), AppSessionRequest{SessionId: "app", App: AppIdClaudeCode})
		if err != nil {
			t.Fatal(err)
		}
		model, err := s.openModelCall(context.Background(), ModelCallRequest{RequestId: "model", Model: "test", Kind: ModelCallKindChat})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// The writer holds no local stream. The registration broadcast is
		// consumed by the replica that does, using the stored row version.
		ev := RegistrationEvent{RegistrationId: s.worker.RegistrationId, ConnectedNodeId: s.server.nodeId, OwnerFeatures: map[string]any{"headless": false, "apps": false, "computerUse": true}, Version: time.Unix(2, 0)}
		NewRegistrationWatcher(NewRegistry(nil, nil), "writer", nil).Observe(context.Background(), ev)
		watcher.Observe(context.Background(), ev)
		synctest.Wait()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "worker_feature_disabled") {
			t.Fatalf("command cancellation: %v", err)
		}
		wire.mu.Lock()
		toolCanceled, appCanceled := false, false
		for _, msg := range wire.sent {
			if msg.GetToolCancel().GetCallId() == "one" {
				toolCanceled = true
			}
			if control := msg.GetAppSessionControl(); control.GetSessionId() == "app" && control.GetAction() == AppSessionActionCancel {
				appCanceled = true
			}
		}
		wire.mu.Unlock()
		if !toolCanceled || !appCanceled {
			t.Fatalf("missing wire cancellations: tool=%v app=%v", toolCanceled, appCanceled)
		}
		select {
		case <-model.done:
			t.Fatal("disabling host features ended inference")
		default:
		}
		// A late read/event cannot undo the newer disable.
		_, err = s.dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "stale", Tool: "workerHost", Action: "exec"}, nil)
		if err == nil || !strings.Contains(err.Error(), "worker_feature_disabled") {
			t.Fatalf("stale settings admitted work: %v", err)
		}
		// Re-enable admits a NEW request, without replaying either canceled one.
		store.policy.Store(&OwnerFeaturePolicy{Features: OwnerFeaturesFromRow(nil), Version: time.Unix(3, 0)})
		go func() {
			_, err := s.dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "two", Tool: "workerHost", Action: "exec"}, nil)
			done <- err
		}()
		synctest.Wait()
		s.handleToolResult(&memqlv1.ToolResult{CallId: "two"})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		wire.mu.Lock()
		starts := 0
		for _, msg := range wire.sent {
			if msg.GetAppSessionStart() != nil {
				starts++
			}
		}
		wire.mu.Unlock()
		if starts != 1 {
			t.Fatalf("re-enable replayed an app: %d starts", starts)
		}
		app.finish(AppSessionOutcome{}, nil)
		model.finish(ModelCallOutcome{}, nil)
	})
}

func TestOwnerFeatureAdmissionFailsClosedAndKeepsOtherFeaturesIndependent(t *testing.T) {
	w := &Worker{}
	policy := OwnerFeaturePolicy{Features: OwnerFeaturesFromRow(nil), Version: time.Unix(1, 0)}
	command, releaseCommand, err := w.enterOwnerFeature(context.Background(), FeatureHeadless, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCommand()
	app, releaseApp, err := w.enterOwnerFeature(context.Background(), FeatureApps, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseApp()
	policy.Version = time.Unix(2, 0)
	policy.Features.Headless = false
	w.ApplyOwnerFeaturePolicy(policy)
	if command.Err() == nil || app.Err() != nil {
		t.Fatal("feature cancellation crossed its scope")
	}
	policy.Version = time.Unix(3, 0)
	policy.Features.Headless = true
	w.ApplyOwnerFeaturePolicy(policy)
	if command.Err() == nil {
		t.Fatal("re-enable revived an old command")
	}
	s, _ := newToolStreamTestSession()
	defer s.cancel()
	store := &featureTestStore{fakeRegistrationStore: &fakeRegistrationStore{}}
	store.unavailable.Store(true)
	s.server.store = store
	if _, err := s.openAppSession(context.Background(), AppSessionRequest{SessionId: "offline", App: AppIdClaudeCode}); err == nil {
		t.Fatal("unreadable settings admitted an app")
	}
}

func TestQueuedHostWorkRechecksFeatureSettingsBeforeDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, wire := newToolStreamTestSession()
		defer s.cancel()
		store := &featureTestStore{fakeRegistrationStore: &fakeRegistrationStore{}}
		store.policy.Store(&OwnerFeaturePolicy{Features: OwnerFeaturesFromRow(nil), Version: time.Unix(1, 0)})
		s.server.store = store
		s.worker.Capabilities = []string{CapabilityHeadless}
		s.worker.Concurrency = map[string]uint32{CapabilityHeadless: 1}
		s.server.registry.Add(s.worker)
		if err := s.worker.Acquire(context.Background(), CapabilityHeadless); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			if err := s.worker.Acquire(context.Background(), CapabilityHeadless); err != nil {
				done <- err
				return
			}
			defer s.worker.Release(CapabilityHeadless)
			_, err := s.dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "queued", Tool: "workerHost", Action: "exec"}, nil)
			done <- err
		}()
		synctest.Wait()
		store.policy.Store(&OwnerFeaturePolicy{Features: OwnerFeatures{Apps: true, ComputerUse: true}, Version: time.Unix(2, 0)})
		s.worker.Release(CapabilityHeadless)
		if err := <-done; err == nil || !strings.Contains(err.Error(), "worker_feature_disabled") {
			t.Fatalf("queued work: %v", err)
		}
		wire.mu.Lock()
		defer wire.mu.Unlock()
		for _, msg := range wire.sent {
			if msg.GetToolDispatch() != nil {
				t.Fatal("queued command reached the machine after disable")
			}
		}
	})
}

func TestOwnerFeatureDisableSurvivesRemoteEventProjectionAndMissedBroadcast(t *testing.T) {
	for _, delivery := range []string{"broadcast", "heartbeat"} {
		t.Run(delivery, func(t *testing.T) {
			s, _ := newToolStreamTestSession()
			defer s.cancel()
			store := &featureTestStore{fakeRegistrationStore: &fakeRegistrationStore{}}
			policy := OwnerFeaturePolicy{Features: OwnerFeaturesFromRow(nil), Version: time.Unix(1, 0)}
			store.policy.Store(&policy)
			s.server.store = store
			s.server.registry.Add(s.worker)
			call, release, err := s.admitOwnerFeature(context.Background(), FeatureComputerUse)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			newer := OwnerFeaturePolicy{Features: OwnerFeatures{Headless: true, Apps: true}, Version: time.Unix(2, 0)}
			store.policy.Store(&newer)
			if delivery == "heartbeat" {
				s.recheckOwnerFeatures(time.Now())
			} else {
				sub := &RegistrationEventSubscriber{logger: slog.Default(), watcher: NewRegistrationWatcher(s.server.registry, s.server.nodeId, nil)}
				sub.handle(events.Event{OriginNodeId: "different-replica", Payload: map[string]any{
					"id": s.worker.RegistrationId, "createdAt": newer.Version.Format(time.RFC3339Nano),
					"payload": map[string]any{"connectedNodeId": s.server.nodeId, "ownerFeatures": map[string]any{"headless": true, "apps": true, "computerUse": false}},
				}})
			}
			if !strings.Contains(fmt.Sprint(context.Cause(call)), "worker_feature_disabled") {
				t.Fatalf("%s did not cancel the held call: %v", delivery, context.Cause(call))
			}
		})
	}
}

// sentToolDispatches counts the ToolDispatch envelopes that reached the
// machine's stream: the only evidence a command may be running on it.
func sentToolDispatches(wire *fakeWorkerStream) int {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	n := 0
	for _, msg := range wire.sent {
		if msg.GetToolDispatch() != nil {
			n++
		}
	}
	return n
}

// storeWithoutFeatures is a Store that cannot report the owner's feature
// settings: embedding the interface hides ReadOwnerFeaturePolicy from the
// OwnerFeatureStore assertion that admission makes.
type storeWithoutFeatures struct{ Store }

// The dispatcher may move a call on to another machine only when it is certain
// nothing ran (design D5). Admission is where that is certain: it decides
// before anything is handed to the stream. A feature withdrawn WHILE a call runs
// is not: the machine holds the command and may have run it, and reading that
// as "nothing ran" would run it a second time on another machine.
func TestAdmissionRefusalIsTypedButMidCallWithdrawalIsNot(t *testing.T) {
	hostCommandsOff := OwnerFeatures{Apps: true, ComputerUse: true}
	exec := func(callId string) *memqlv1.ToolDispatch {
		return &memqlv1.ToolDispatch{CallId: callId, Tool: "workerHost", Action: "exec"}
	}
	session := func(t *testing.T, features OwnerFeatures, version time.Time) (*streamSession, *fakeWorkerStream, *featureTestStore) {
		t.Helper()
		s, wire := newToolStreamTestSession()
		t.Cleanup(s.cancel)
		store := &featureTestStore{fakeRegistrationStore: &fakeRegistrationStore{}}
		store.policy.Store(&OwnerFeaturePolicy{Features: features, Version: version})
		s.server.store = store
		return s, wire, store
	}

	t.Run("Host commands off refuses at admission with nothing sent", func(t *testing.T) {
		s, wire, _ := session(t, hostCommandsOff, time.Unix(1, 0))
		_, err := s.dispatch(context.Background(), exec("off"), nil)
		if code, ok := RefusedBeforeSend(err); !ok || code != "worker_feature_disabled" {
			t.Fatalf("RefusedBeforeSend(%v) = (%q, %v), want (worker_feature_disabled, true)", err, code, ok)
		}
		if n := sentToolDispatches(wire); n != 0 {
			t.Fatalf("%d dispatch(es) reached the machine of a refused call", n)
		}
		// The sentence the owner reads is the one it always was.
		if !strings.Contains(err.Error(), "worker_feature_disabled") || !strings.Contains(err.Error(), "Fleet settings") {
			t.Fatalf("the refusal lost its words: %v", err)
		}
		// Layers above wrap what this returns; the type must survive that.
		if code, ok := RefusedBeforeSend(fmt.Errorf("dispatching: %w", err)); !ok || code != "worker_feature_disabled" {
			t.Fatalf("a wrapped refusal is no longer recognised: (%q, %v)", code, ok)
		}
	})

	t.Run("a setting that cannot be read refuses at admission with nothing sent", func(t *testing.T) {
		s, wire, store := session(t, OwnerFeaturesFromRow(nil), time.Unix(1, 0))
		store.unavailable.Store(true)
		_, err := s.dispatch(context.Background(), exec("unread"), nil)
		if code, ok := RefusedBeforeSend(err); !ok || code != "worker_feature_unavailable" {
			t.Fatalf("RefusedBeforeSend(%v) = (%q, %v), want (worker_feature_unavailable, true)", err, code, ok)
		}
		if !strings.Contains(err.Error(), "settings unavailable") || errors.Unwrap(err) == nil {
			t.Fatalf("the read failure is no longer the refusal's cause: %v", err)
		}
		if n := sentToolDispatches(wire); n != 0 {
			t.Fatalf("%d dispatch(es) reached the machine of a refused call", n)
		}
	})

	t.Run("a setting with no version refuses at admission", func(t *testing.T) {
		s, wire, _ := session(t, OwnerFeaturesFromRow(nil), time.Time{})
		_, err := s.dispatch(context.Background(), exec("unversioned"), nil)
		if code, ok := RefusedBeforeSend(err); !ok || code != "worker_feature_unavailable" {
			t.Fatalf("RefusedBeforeSend(%v) = (%q, %v), want (worker_feature_unavailable, true)", err, code, ok)
		}
		if n := sentToolDispatches(wire); n != 0 {
			t.Fatalf("%d dispatch(es) reached the machine of a refused call", n)
		}
	})

	t.Run("a store that cannot report settings refuses at admission", func(t *testing.T) {
		s, wire, _ := session(t, OwnerFeaturesFromRow(nil), time.Unix(1, 0))
		s.server.store = storeWithoutFeatures{Store: s.server.store}
		_, err := s.dispatch(context.Background(), exec("blind"), nil)
		if code, ok := RefusedBeforeSend(err); !ok || code != "worker_feature_unavailable" {
			t.Fatalf("RefusedBeforeSend(%v) = (%q, %v), want (worker_feature_unavailable, true)", err, code, ok)
		}
		if n := sentToolDispatches(wire); n != 0 {
			t.Fatalf("%d dispatch(es) reached the machine of a refused call", n)
		}
	})

	t.Run("an app session refused at admission is typed the same way", func(t *testing.T) {
		s, _, _ := session(t, OwnerFeatures{Headless: true, ComputerUse: true}, time.Unix(1, 0))
		_, err := s.openAppSession(context.Background(), AppSessionRequest{SessionId: "off", App: AppIdClaudeCode})
		if code, ok := RefusedBeforeSend(err); !ok || code != "worker_feature_disabled" {
			t.Fatalf("RefusedBeforeSend(%v) = (%q, %v), want (worker_feature_disabled, true)", err, code, ok)
		}
	})

	t.Run("a withdrawal after the send is a plain error", func(t *testing.T) {
		s, wire, _ := session(t, OwnerFeaturesFromRow(nil), time.Unix(1, 0))
		done := make(chan error, 1)
		go func() {
			_, err := s.dispatch(context.Background(), exec("running"), nil)
			done <- err
		}()
		deadline := time.Now().Add(2 * time.Second)
		for sentToolDispatches(wire) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the dispatch never reached the machine")
			}
			time.Sleep(time.Millisecond)
		}
		// The owner switches Host commands off while the command is on the machine.
		s.worker.ApplyOwnerFeaturePolicy(OwnerFeaturePolicy{Features: hostCommandsOff, Version: time.Unix(2, 0)})
		var err error
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the withdrawal did not end the call")
		}
		if err == nil || !strings.Contains(err.Error(), "worker_feature_disabled") {
			t.Fatalf("the call was not ended by the withdrawal: %v", err)
		}
		if code, ok := RefusedBeforeSend(err); ok {
			t.Fatalf("a withdrawal AFTER the send reads as a refusal before send (%q): the command may have run, and the dispatcher would run it again on another machine", code)
		}
		if n := sentToolDispatches(wire); n != 1 {
			t.Fatalf("%d dispatch(es) reached the machine, want the one that was running", n)
		}
	})
}
