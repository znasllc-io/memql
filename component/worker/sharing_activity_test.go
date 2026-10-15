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
	subjects := SharingSubjects{UserIds: []string{"v1:identity:user:ana"}, GroupIds: []string{"design"}}
	for _, call := range []string{"one", "two"} {
		if _, err := session.openModelCall(context.Background(), ModelCallRequest{RequestId: call, Model: "local", Kind: ModelCallKindChat, SharingSubjects: subjects, Messages: []ModelCallMessage{{Role: "user", Content: "private prompt"}}}); err != nil {
			t.Fatal(err)
		}
	}
	counts := session.sharingActivity()
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
