package pipelinesteps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

func cleanupTestSecret(i int) Secret {
	s := BuildSecret(rtConfig(), rtRun(), fmt.Sprintf("mp-%024x", i), rtCloneToken)
	s.Metadata.Annotations[AnnotRunDeadline] = rtT0.Add(-rtConfig().JobTTL - time.Hour).Format(time.RFC3339Nano)
	s.Metadata.CreationTimestamp = rtT0.Add(-24 * time.Hour)
	return s
}

func TestMaintenanceCleansAtStartupAndOnTimerWithoutBuilds(t *testing.T) {
	h := newRunnerHarness(t)
	first, later := cleanupTestSecret(1), cleanupTestSecret(2)
	h.c.putSecret(first)
	m := NewMaintenance(h.r)
	m.interval, m.continuationDelay = time.Millisecond, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.run(ctx, func() {}) }()
	t.Cleanup(cancel)
	waitGone := func(name string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for h.c.hasSecret(name) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if h.c.hasSecret(name) {
			t.Fatal("idle maintenance left orphan", name)
		}
	}
	waitGone(first.Metadata.Name)
	h.c.putSecret(later)
	waitGone(later.Metadata.Name)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
	if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
		t.Fatal("maintenance started a build")
	}
}

func TestReaperDeleteCannotRemoveReplacedOrNewlyOwnedSecret(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			h := newRunnerHarness(t)
			s := cleanupTestSecret(1)
			h.c.putSecret(s)
			rows, _, err := h.c.kube.ManagedSecrets(context.Background(), 100, "")
			if err != nil || len(rows) != 1 {
				t.Fatalf("list: %v %v", rows, err)
			}
			if replacement {
				s.Metadata.UID = "replacement-object"
			} else {
				s.Metadata.OwnerReferences = []OwnerReference{{Kind: "Job", Name: "new-owner", UID: "owner-uid"}}
			}
			h.c.putSecret(s)
			if err := h.c.kube.DeleteObservedSecret(context.Background(), rows[0]); err == nil {
				t.Fatal("stale observation deleted a changed Secret")
			}
			if !h.c.hasSecret(s.Metadata.Name) {
				t.Fatal("changed Secret lost")
			}
			rows, _, err = h.c.kube.ManagedSecrets(context.Background(), 100, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := h.c.kube.DeleteObservedSecret(context.Background(), rows[0]); err != nil {
				t.Fatal("fresh identity refused", err)
			}
			if err := h.c.kube.DeleteObservedSecret(context.Background(), rows[0]); err != nil {
				t.Fatal("absent resource is not idempotent", err)
			}
		})
	}
}

func TestReaperRestartsExpiredPaginationAndRetriesAPIFailure(t *testing.T) {
	h := newRunnerHarness(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if r.URL.Query().Get("continue") != "expired" {
				t.Error("continuation lost")
			}
			rtAnswer(w, kubeStatus(410, "Expired", "snapshot expired"))
			return
		}
		if r.URL.Query().Get("continue") != "" {
			t.Error("expired continuation retried")
		}
		h.c.serve(w, r)
	}))
	defer srv.Close()
	h.r.kube = NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), "steps-ns")
	h.r.reapCursor = "expired"
	if _, more, err := h.r.reap(context.Background()); !more || err == nil {
		t.Fatalf("expiration not retryable: %v %v", more, err)
	}
	h.c.putSecret(cleanupTestSecret(1))
	if n, more, err := h.r.reap(context.Background()); n != 1 || more || err != nil {
		t.Fatalf("fresh scan failed: %d %v %v", n, more, err)
	}
}

func TestMaintenanceCancellationEndsAnUnavailableAPICall(t *testing.T) {
	h := newRunnerHarness(t)
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()
	h.r.kube = NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), "steps-ns")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- NewMaintenance(h.r).run(ctx, func() {}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no API call")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance outlived shutdown")
	}
}
