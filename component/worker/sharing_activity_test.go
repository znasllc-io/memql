package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestSharingActivityFollowsActualCallLifetimeWithoutContent(t *testing.T) {
	session, _ := newToolStreamTestSession()
	defer session.cancel()
	subjects := SharingSubjects{UserIds: []string{"v1:identity:user:ana"}, GroupIds: []string{"design"}, Borrowed: true}
	for _, call := range []string{"one", "two"} {
		if _, err := session.openModelCall(context.Background(), ModelCallRequest{RequestId: call, Model: "local", Kind: ModelCallKindChat, SharingSubjects: subjects, Messages: []ModelCallMessage{{Role: "user", Content: "private prompt"}}}); err != nil {
			t.Fatal(err)
		}
	}
	counts := session.sharingActivity()
	if counts["sharedCalls"] != 2 || counts["ownerCalls"] != 0 {
		t.Fatalf("overlapping grants must count each shared call once: %+v", counts)
	}
	for _, kind := range []string{"users", "groups"} {
		if counts[kind].([]map[string]any)[0]["activeCalls"] != 2 {
			t.Fatalf("missing counts: %+v", counts)
		}
	}
	raw, _ := json.Marshal(counts)
	if strings.Contains(string(raw), "private prompt") || strings.Contains(string(raw), "messages") {
		t.Fatal("activity exposed call content")
	}
	// Terminal events detach immediately; a paused grant has no cancellation
	// path into these handles, so an in-flight call keeps its original subjects.
	session.handleModelCallEnd(&memqlv1.ModelCallEnd{RequestId: "one", FinishReason: ModelFinishStop})
	if session.sharingActivity()["users"].([]map[string]any)[0]["activeCalls"] != 1 {
		t.Fatal("finished call stayed counted")
	}
	session.handleModelCallEnd(&memqlv1.ModelCallEnd{RequestId: "two", FinishReason: ModelFinishStop})
	if len(session.sharingActivity()["groups"].([]map[string]any)) != 0 {
		t.Fatal("last finished call stayed counted")
	}
	select {
	case <-session.sharingChanged:
	default:
		t.Fatal("call changes did not notify the publisher")
	}
}

func TestMachineActivitySeparatesOwnerWorkFromNamedAndClusterShares(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, _ := newToolStreamTestSession()
		defer session.cancel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		commandCtx, cancelCommand := context.WithCancel(ctx)
		defer cancelCommand()
		commandDone := make(chan error, 1)
		go func() {
			_, err := session.dispatch(commandCtx, &memqlv1.ToolDispatch{CallId: "command", Tool: "workerHost", Action: "exec"}, nil)
			commandDone <- err
		}()
		synctest.Wait()
		if _, err := session.openAppSession(ctx, AppSessionRequest{SessionId: "app", App: "claude-code"}); err != nil {
			t.Fatal(err)
		}
		for _, call := range []ModelCallRequest{
			{RequestId: "own", Model: "local", Kind: ModelCallKindChat},
			{RequestId: "named", Model: "local", Kind: ModelCallKindChat, SharingSubjects: SharingSubjects{Borrowed: true, UserIds: []string{"ana"}, GroupIds: []string{"design", "team"}}},
			{RequestId: "cluster", Model: "local", Kind: ModelCallKindChat, SharingSubjects: SharingSubjects{Borrowed: true}},
		} {
			if _, err := session.openModelCall(ctx, call); err != nil {
				t.Fatal(err)
			}
		}
		assertCounts := func(owner, shared int) {
			t.Helper()
			got := session.sharingActivity()
			if got["ownerCalls"] != owner || got["sharedCalls"] != shared {
				t.Fatalf("want owner=%d shared=%d; got %+v", owner, shared, got)
			}
		}
		assertCounts(3, 2)
		// A canceled command and a finished app leave the owner's count;
		// an admitted shared call remains until its own terminal event.
		cancelCommand()
		if err := <-commandDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "app"})
		assertCounts(1, 2)
		// Cancelling the remaining callers ends their local handles even if
		// the machine never sends End. The command has its own context above:
		// cancelling every caller while expecting shared calls to remain was
		// relying on the old leaked-handle behavior.
		cancel()
		synctest.Wait()
		assertCounts(0, 0)
		for _, request := range []string{"own", "named", "cluster"} {
			session.handleModelCallEnd(&memqlv1.ModelCallEnd{RequestId: request, FinishReason: ModelFinishStop})
		}
		assertCounts(0, 0)
	})
}

type activityPublishStore struct {
	Store
	attempts atomic.Int64
	fail     atomic.Bool
}

func (s *activityPublishStore) UpdateSharingActivity(_ context.Context, _, _, _ string, _ map[string]any) error {
	s.attempts.Add(1)
	if s.fail.Load() {
		return errors.New("temporarily unavailable")
	}
	return nil
}

func TestSharingActivityCoalescesRetriesAndStopsAfterDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &activityPublishStore{}
		store.fail.Store(true)
		s := newHeartbeatTestSession(store, time.Now)
		s.server.registry.Add(s.worker)
		done := make(chan struct{})
		go func() { s.runSharingActivity(); close(done) }()
		synctest.Wait()
		for i := 0; i < 100; i++ {
			s.markSharingChanged()
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if store.attempts.Load() != 1 {
			t.Fatalf("burst made %d writes", store.attempts.Load())
		}
		store.fail.Store(false)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if store.attempts.Load() != 2 {
			t.Fatalf("failed report was not repaired: %d writes", store.attempts.Load())
		}
		s.cancel()
		<-done
		s.markSharingChanged()
		time.Sleep(20 * time.Second)
		if store.attempts.Load() != 2 {
			t.Fatal("disconnected stream kept publishing")
		}
	})
}

func TestBorrowedWorkbenchCountsOnceAndKeepsItsAudienceSeparateFromModels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, _ := newToolStreamTestSession()
		defer session.cancel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = ContextWithToolSharing(ctx, SharingSubjects{Borrowed: true, UserIds: []string{"borrower"}, GroupIds: []string{"group-a", "group-b"}})
		done := make(chan error, 1)
		go func() {
			_, err := session.dispatch(ctx, &memqlv1.ToolDispatch{CallId: "borrowed-workspace", Tool: "workerWorkbench", Action: "exec", Workbench: &memqlv1.WorkbenchConstraint{CallerUserId: "borrower"}}, nil)
			done <- err
		}()
		synctest.Wait()
		counts := session.sharingActivity()
		if counts["sharedCalls"] != 1 || counts["ownerCalls"] != 0 {
			t.Fatalf("overlapping workbench grants inflated counts: %+v", counts)
		}
		if len(counts["users"].([]map[string]any)) != 0 {
			t.Fatal("workbench work appeared as model inference")
		}
		wb := counts["workbench"].(map[string]any)
		if len(wb["groups"].([]map[string]any)) != 2 || wb["users"].([]map[string]any)[0]["activeCalls"] != 1 {
			t.Fatal("workbench audience activity was not measured")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if session.sharingActivity()["sharedCalls"] != 0 {
			t.Fatal("cancelled work remained active")
		}
	})
}
