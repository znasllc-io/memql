package emailrules

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestFiringGateSlotsLeaveThePoolAConnectionForTheRecord: however the lock
// pool is capped, the slots leave at least half of it for the records' engine
// calls -- which is what makes "every holder waits for a second connection"
// impossible when the two share a pool. A pool of one gets no slots at all.
func TestFiringGateSlotsLeaveThePoolAConnectionForTheRecord(t *testing.T) {
	for _, c := range []struct{ maxOpen, want int }{
		{0, firingGateMaxSlots}, // unlimited
		{1, 0},
		{2, 1},
		{3, 1},
		{4, 2}, // the base deploy's cap, main and direct alike
		{5, 2},
		{100, firingGateMaxSlots},
	} {
		got := firingGateSlots(c.maxOpen)
		if got != c.want {
			t.Errorf("firingGateSlots(%d) = %d, want %d", c.maxOpen, got, c.want)
		}
		if c.maxOpen > 0 && got > 0 && c.maxOpen-got < c.maxOpen/2 {
			t.Errorf("firingGateSlots(%d) = %d leaves fewer than half the pool for the records", c.maxOpen, got)
		}
	}
}

// TestKeyedMutexSerialisesAKeyAndNothingElse: a second holder of one key waits
// for the first; another key does not; a waiter whose context ends gives up;
// and a key nobody holds or waits for is forgotten.
func TestKeyedMutexSerialisesAKeyAndNothingElse(t *testing.T) {
	k := newKeyedMutex()
	ctx := context.Background()

	releaseA, err := k.lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := k.lock(ctx, "b")
	if err != nil {
		t.Fatalf("another key waited on the first: %v", err)
	}

	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := k.lock(short, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second holder of a held key: %v, want it to wait until its context ended", err)
	}

	got := make(chan func(), 1)
	go func() {
		release, err := k.lock(ctx, "a")
		if err != nil {
			t.Error(err)
		}
		got <- release
	}()
	select {
	case <-got:
		t.Fatal("the second holder of \"a\" did not wait for the first")
	case <-time.After(50 * time.Millisecond):
	}
	releaseA()
	var releaseA2 func()
	select {
	case releaseA2 = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("releasing \"a\" did not hand it to its waiter")
	}
	releaseA2()
	releaseB()
	if n := k.size(); n != 0 {
		t.Fatalf("%d keys remain after every holder released and every waiter left", n)
	}
}
