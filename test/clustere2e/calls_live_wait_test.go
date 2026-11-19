package clustere2e

// calls_live_wait_test.go -- the wait TestTwoPeopleHoldACallThroughTheFrontDoor
// (calls_live_test.go, under the clustere2e tag) makes for its calls switch to reach the
// edge replicas, as a pure function with its own test in the default build: the live leg
// runs once, with the owner, so the rule it asserts is proved here first.
//
// THE RULE. Each edge replica learns of a write to a site on its own, through its own
// subscription to the change feed (site_edge_invalidation_test.go), so what the cluster
// promises is a BOUND per replica -- every one serves the change within `within` of the
// write -- and not that every replica has it by the moment the first one does. The wait
// asks each named replica until it serves, every one against the same deadline counted
// from the write, and fails only past the bound, naming the replicas that had not served
// and how many had.

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// replicaWait is what a wait for a change to reach every replica answers: how long each
// replica took to serve it, counted from the change.
type replicaWait map[string]time.Duration

// waitForEveryReplica asks each replica, in rounds every apart, whether it serves the
// change made at since, until every one does or within of since has passed (the file
// note's rule). A replica that has served is not asked again: an invalidated cache holds
// the new value. The last round is asked at the bound itself, and a replica that has not
// served by then fails the wait, with the failure naming the bound, how many replicas
// had served and after how long, and which had not. An error from asking comes back at
// once, since a replica that cannot be asked is not a replica that lags. clock and sleep
// are the wait's own, so a test can script them.
func waitForEveryReplica(clock func() time.Time, sleep func(time.Duration), since time.Time, within, every time.Duration,
	replicas []string, serves func(replica string) (bool, error)) (replicaWait, error) {
	took := replicaWait{}
	deadline := since.Add(within)
	for {
		for _, replica := range replicas {
			if _, served := took[replica]; served {
				continue
			}
			ok, err := serves(replica)
			if err != nil {
				return took, fmt.Errorf("asking %s whether it serves the change: %w", replica, err)
			}
			if ok {
				took[replica] = clock().Sub(since)
			}
		}
		if len(took) == len(replicas) {
			return took, nil
		}
		remaining := deadline.Sub(clock())
		if remaining <= 0 {
			var missing []string
			for _, replica := range replicas {
				if _, served := took[replica]; !served {
					missing = append(missing, replica)
				}
			}
			return took, fmt.Errorf("%d of %d replicas served the change within %s of it (%s); %s did not",
				len(took), len(replicas), within, took.describe(), strings.Join(missing, ", "))
		}
		sleep(min(every, remaining))
	}
}

// edgeServesCalls reads one edge replica's answer to a deployable's session route asked
// with no cookie: the edge's own 404 is calls still off on that replica, and the bff's
// 401, which only a forwarded request reaches, is calls on. Anything else is neither, and
// an error rather than a "not yet", so the wait does not spend its bound on it.
func edgeServesCalls(status int, body string) (bool, error) {
	switch body = strings.TrimSpace(body); {
	case status == http.StatusNotFound && body == `{"error":"not_found"}`:
		return false, nil
	case status == http.StatusUnauthorized && body == `{"error":"no_session"}`:
		return true, nil
	}
	return false, fmt.Errorf("answered %d %s, which is neither calls off (the edge's 404 not_found) nor on (the bff's 401 no_session)", status, body)
}

// describe is which replicas served and after how long, by name, or "none".
func (w replicaWait) describe() string {
	if len(w) == 0 {
		return "none"
	}
	names := make([]string, 0, len(w))
	for name := range w {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s after %s", name, w[name]))
	}
	return strings.Join(parts, ", ")
}

// scriptedReplicas is a scripted cluster: a clock the wait's sleeps move, and replicas
// that each start serving the change a scripted time after it, or never.
type scriptedReplicas struct {
	now, change time.Time
	servesAfter map[string]time.Duration // absent: never serves
	failing     map[string]error         // asking this replica fails
	asked       map[string]int
}

func newScriptedReplicas(servesAfter map[string]time.Duration) *scriptedReplicas {
	change := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	return &scriptedReplicas{now: change, change: change, servesAfter: servesAfter, failing: map[string]error{}, asked: map[string]int{}}
}

func (s *scriptedReplicas) clock() time.Time      { return s.now }
func (s *scriptedReplicas) sleep(d time.Duration) { s.now = s.now.Add(d) }

func (s *scriptedReplicas) serves(replica string) (bool, error) {
	s.asked[replica]++
	if err := s.failing[replica]; err != nil {
		return false, err
	}
	after, ever := s.servesAfter[replica]
	return ever && !s.now.Before(s.change.Add(after)), nil
}

// wait is the wait over every replica the script names, in name order, asked every 250 ms
// as the live leg asks.
func (s *scriptedReplicas) wait(within time.Duration) (replicaWait, error) {
	seen := map[string]bool{}
	for name := range s.servesAfter {
		seen[name] = true
	}
	for name := range s.failing {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return waitForEveryReplica(s.clock, s.sleep, s.change, within, 250*time.Millisecond, names, s.serves)
}

// TestTheSwitchIsWaitedForOnEveryReplicaWithinTheBound: the wait holds every replica to
// the one bound, counted from the change, and to nothing tighter.
func TestTheSwitchIsWaitedForOnEveryReplicaWithinTheBound(t *testing.T) {
	const bound = 10 * time.Second

	// THE CASE THE LIVE LEG MUST NOT FAIL: the replica the next request reaches learns of
	// the change seconds after the first one did, inside the bound.
	t.Run("a replica that lags the first one is waited for, inside the bound", func(t *testing.T) {
		s := newScriptedReplicas(map[string]time.Duration{"edge-a": 200 * time.Millisecond, "edge-b": 3 * time.Second})
		took, err := s.wait(bound)
		if err != nil {
			t.Fatalf("a replica 3s behind the first, inside the %s bound, failed the wait: %v", bound, err)
		}
		if took["edge-a"] >= took["edge-b"] || took["edge-b"] < 3*time.Second || took["edge-b"] >= bound {
			t.Fatalf("took = %v, want edge-a first and edge-b from 3s, both inside %s", took, bound)
		}
	})

	t.Run("the bound counts from the change, not from the first replica to serve", func(t *testing.T) {
		s := newScriptedReplicas(map[string]time.Duration{"edge-a": 9 * time.Second, "edge-b": 9500 * time.Millisecond})
		if _, err := s.wait(bound); err != nil {
			t.Fatalf("two replicas inside the bound failed the wait: %v", err)
		}
	})

	t.Run("a replica that serves only past the bound fails the wait, at the bound, by name", func(t *testing.T) {
		s := newScriptedReplicas(map[string]time.Duration{"edge-a": 200 * time.Millisecond, "edge-b": 12 * time.Second})
		_, err := s.wait(bound)
		if err == nil {
			t.Fatal("a replica that served 12s after the change passed a 10s bound")
		}
		for _, want := range []string{"1 of 2", "edge-b", "10s"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the failure %q does not say %q", err, want)
			}
		}
		if waited := s.now.Sub(s.change); waited < bound || waited >= bound+250*time.Millisecond {
			t.Errorf("the wait gave up %s after the change, want at the %s bound", waited, bound)
		}
	})

	t.Run("no replica serving fails the wait, naming them all", func(t *testing.T) {
		s := newScriptedReplicas(map[string]time.Duration{})
		_, err := waitForEveryReplica(s.clock, s.sleep, s.change, bound, 250*time.Millisecond, []string{"edge-a", "edge-b"}, s.serves)
		if err == nil || !strings.Contains(err.Error(), "0 of 2") || !strings.Contains(err.Error(), "edge-a, edge-b") {
			t.Fatalf("err = %v, want 0 of 2 with both named", err)
		}
	})

	t.Run("a replica that has served is not asked again", func(t *testing.T) {
		s := newScriptedReplicas(map[string]time.Duration{"edge-a": 0, "edge-b": 2 * time.Second})
		if _, err := s.wait(bound); err != nil {
			t.Fatal(err)
		}
		if s.asked["edge-a"] != 1 {
			t.Errorf("edge-a was asked %d times after it served at once, want 1", s.asked["edge-a"])
		}
	})

	// A REPLICA THAT CANNOT BE ASKED IS NOT A REPLICA THAT LAGS: the error comes back at
	// once, with the replica's name, rather than as a timeout ten seconds later.
	t.Run("an error asking a replica is returned at once", func(t *testing.T) {
		s := newScriptedReplicas(map[string]time.Duration{"edge-a": 0})
		s.failing["edge-b"] = errors.New("connection refused")
		_, err := s.wait(bound)
		if err == nil || !strings.Contains(err.Error(), "edge-b") || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("err = %v, want edge-b's own failure", err)
		}
		if waited := s.now.Sub(s.change); waited != 0 {
			t.Errorf("the wait went on %s after a replica could not be asked", waited)
		}
	})
}

// TestAnEdgeReplicasAnswerSaysWhetherItServesCalls: the edge's own 404 is calls off on
// that replica, the bff's 401 is calls on, and nothing else is read as either.
func TestAnEdgeReplicasAnswerSaysWhetherItServesCalls(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		serves bool
		fails  bool
	}{
		{http.StatusNotFound, `{"error":"not_found"}`, false, false},
		{http.StatusUnauthorized, "{\"error\":\"no_session\"}\n", true, false},
		{http.StatusNotFound, "404 page not found", false, true},
		{http.StatusUnauthorized, `{"error":"invalid_key"}`, false, true},
		{http.StatusBadGateway, `{"error":"failed"}`, false, true},
		{http.StatusOK, `{"kind":"host"}`, false, true},
	} {
		serves, err := edgeServesCalls(tc.status, tc.body)
		if serves != tc.serves || (err != nil) != tc.fails {
			t.Errorf("%d %q read as serves=%v err=%v, want serves=%v and an error %v", tc.status, tc.body, serves, err, tc.serves, tc.fails)
		}
	}
}
