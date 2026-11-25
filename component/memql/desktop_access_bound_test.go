package memql

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// The bound on hold-line writes (desktop_access_bound.go): a burst of three
// per person on a replica, then one a minute.

type holdClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *holdClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *holdClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newHoldClock() *holdClock {
	return &holdClock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
}

// boundEngine is holdEngine with a limiter on a clock the test moves.
func boundEngine() (*MemQLEngine, *holdClock) {
	e := holdEngine(auth.RoleWriter)
	clock := newHoldClock()
	e.accessHolds.Store(newAccessHoldLimiter(clock.now))
	return e, clock
}

func TestTheHoldBoundIsABurstOfThreeThenOneAMinute(t *testing.T) {
	clock := newHoldClock()
	l := newAccessHoldLimiter(clock.now)
	for i := 0; i < accessHoldBurst; i++ {
		if !l.allow("ana") {
			t.Fatalf("write %d of the burst was refused", i+1)
		}
	}
	if l.allow("ana") {
		t.Fatal("a fourth write at once was allowed")
	}
	clock.advance(accessHoldRefill - time.Second)
	if l.allow("ana") {
		t.Fatal("a write before the refill interval was allowed")
	}
	clock.advance(time.Second)
	if !l.allow("ana") {
		t.Fatal("the write a refill interval later was refused")
	}
	if l.allow("ana") {
		t.Fatal("a refill interval buys one write, not two")
	}
}

// Calls passing no reference write at most three lines; the ones past the
// bound are held with no reference and no digest, the answer a failed write
// gives.
func TestCallsWithNoReferenceWriteAtMostTheBurst(t *testing.T) {
	rec := installRecorder(t)
	e, _ := boundEngine()
	for i := 0; i < 5; i++ {
		got, err := e.desktopAccess(personContext("ana"), "ana", "", "")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case got.Admitted:
			t.Fatalf("call %d: answer = %+v, want held", i+1, got)
		case i < accessHoldBurst && !accessReferencePattern.MatchString(got.Reference):
			t.Fatalf("call %d: answer = %+v, want a reference", i+1, got)
		case i >= accessHoldBurst && (got.Reference != "" || got.ReasonsDigest != ""):
			t.Fatalf("call %d past the bound: answer = %+v, want no reference and no digest", i+1, got)
		}
	}
	if len(rec.lines) != accessHoldBurst {
		t.Fatalf("lines = %d, want %d", len(rec.lines), accessHoldBurst)
	}
}

// Past the bound, a valid passed reference comes back with the digest the
// caller passed, unchanged, and nothing is written; after the refill interval
// the next check sees the changed reasons and writes them under it.
func TestPastTheBoundAPassedReferenceIsKeptAndTheChangeWaits(t *testing.T) {
	rec := installRecorder(t)
	e, clock := boundEngine()
	first, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || first.Reference == "" {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	for i := 1; i < accessHoldBurst; i++ {
		if _, err := e.desktopAccess(personContext("ana"), "ana", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	// The reasons change while the bucket is empty.
	e.providers.SetClusterAccountGate(func(context.Context, string) bool { return true })
	held, err := e.desktopAccess(personContext("ana"), "ana", first.Reference, first.ReasonsDigest)
	if err != nil {
		t.Fatal(err)
	}
	if held.Reference != first.Reference || held.ReasonsDigest != first.ReasonsDigest {
		t.Fatalf("past the bound: answer = %+v, want the passed reference and digest unchanged", held)
	}
	if len(rec.lines) != accessHoldBurst {
		t.Fatalf("lines = %d, want nothing written past the bound", len(rec.lines))
	}

	clock.advance(accessHoldRefill)
	again, err := e.desktopAccess(personContext("ana"), "ana", held.Reference, held.ReasonsDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.lines) != accessHoldBurst+1 {
		t.Fatalf("lines = %d, want the changed reasons written after the refill", len(rec.lines))
	}
	line := rec.lines[len(rec.lines)-1]
	if line.Reference != first.Reference || again.Reference != first.Reference {
		t.Fatalf("line = %+v, answer = %+v, want the same reference", line, again)
	}
	if again.ReasonsDigest == first.ReasonsDigest || again.ReasonsDigest != personAccessDigest("ana", line.Reasons) {
		t.Fatalf("digest %q, want the written reasons' digest", again.ReasonsDigest)
	}
}

// A check passing back its reference with unchanged reasons writes nothing and
// spends nothing, however often it runs.
func TestTheSteadyStateSpendsNothing(t *testing.T) {
	rec := installRecorder(t)
	e, _ := boundEngine()
	first, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || first.Reference == "" {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	for i := 0; i < 20; i++ {
		got, err := e.desktopAccess(personContext("ana"), "ana", first.Reference, first.ReasonsDigest)
		if err != nil || got.Reference != first.Reference || got.ReasonsDigest != first.ReasonsDigest {
			t.Fatalf("check %d: answer = %+v, err = %v", i+1, got, err)
		}
	}
	// The rest of the burst is still there.
	for i := 1; i < accessHoldBurst; i++ {
		if got, _ := e.desktopAccess(personContext("ana"), "ana", "", ""); got.Reference == "" {
			t.Fatalf("write %d of the burst was refused after the steady state", i+1)
		}
	}
	if got, _ := e.desktopAccess(personContext("ana"), "ana", "", ""); got.Reference != "" {
		t.Fatalf("answer = %+v, want the bound reached", got)
	}
	if len(rec.lines) != accessHoldBurst {
		t.Fatalf("lines = %d, want %d", len(rec.lines), accessHoldBurst)
	}
}

// The bound is per person: one person past it does not stop another's line.
func TestTheHoldBoundIsPerPerson(t *testing.T) {
	rec := installRecorder(t)
	e, _ := boundEngine()
	for i := 0; i <= accessHoldBurst; i++ {
		if _, err := e.desktopAccess(personContext("ana"), "ana", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.desktopAccess(personContext("ben"), "ben", "", "")
	if err != nil || !accessReferencePattern.MatchString(got.Reference) {
		t.Fatalf("ben: answer = %+v, err = %v, want his own line", got, err)
	}
	if n := len(rec.lines); n != accessHoldBurst+1 || rec.lines[n-1].UserId != "ben" {
		t.Fatalf("lines = %+v", rec.lines)
	}
}

// An entry is dropped once its bucket is full again: it says nothing a fresh
// entry would not.
func TestTheHoldBoundForgetsAPersonOnceTheirBucketIsFull(t *testing.T) {
	clock := newHoldClock()
	l := newAccessHoldLimiter(clock.now)
	l.allow("ana")
	clock.advance(accessHoldRefill / 2)
	l.allow("ben")
	if n := l.size(); n != 2 {
		t.Fatalf("size = %d, want both remembered while refilling", n)
	}
	clock.advance(accessHoldRefill / 2)
	l.allow("cy")
	if n := l.size(); n != 2 {
		t.Fatalf("size = %d, want ana forgotten once her bucket is full", n)
	}
}

// The limiter holds at most its cap of people. A newcomer while every entry
// is still refilling is past the bound; once entries are full again they make
// room.
func TestTheHoldBoundKeepsAtMostItsCapOfPeople(t *testing.T) {
	clock := newHoldClock()
	l := newAccessHoldLimiter(clock.now)
	l.max = 2
	if !l.allow("ana") || !l.allow("ben") {
		t.Fatal("the first two people were refused")
	}
	if l.allow("cy") {
		t.Fatal("a third person was let past a cap of two")
	}
	if n := l.size(); n != 2 {
		t.Fatalf("size = %d, want the cap", n)
	}
	clock.advance(accessHoldRefill)
	if !l.allow("cy") {
		t.Fatal("cy was refused after the others' buckets filled")
	}
	if n := l.size(); n != 1 {
		t.Fatalf("size = %d, want only cy", n)
	}
}
