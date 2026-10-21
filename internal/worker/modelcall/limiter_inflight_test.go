package modelcall

import "testing"

func TestInFlightByModelCountsEverySharedSlot(t *testing.T) {
	l := NewLimiter()
	for _, id := range []string{"a", "a", "b"} {
		if !l.tryAcquire(id, 0, 0) {
			t.Fatalf("acquire %s refused", id)
		}
	}
	got := l.InFlightByModel()
	if got["a"] != 2 || got["b"] != 1 || len(got) != 2 {
		t.Fatalf("in flight = %v", got)
	}
	l.release("b")
	if got := l.InFlightByModel(); got["b"] != 0 || len(got) != 1 {
		t.Fatalf("after release = %v", got)
	}
	if (*Limiter)(nil).InFlightByModel() != nil {
		t.Fatal("a nil limiter reports calls")
	}
}
