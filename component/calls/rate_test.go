package calls

import (
	"testing"
	"time"
)

func TestCountJoinAttemptHoldsAFixedWindow(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		w     JoinWindow
		now   time.Time
		limit int
		next  JoinWindow
		ok    bool
		retry time.Duration
	}{
		{"no window yet opens one", JoinWindow{}, start, 30,
			JoinWindow{StartedAt: start, Count: 1}, true, 0},
		{"inside the window counts", JoinWindow{StartedAt: start, Count: 4}, start.Add(10 * time.Second), 30,
			JoinWindow{StartedAt: start, Count: 5}, true, 0},
		{"the last place in the window", JoinWindow{StartedAt: start, Count: 29}, start.Add(59 * time.Second), 30,
			JoinWindow{StartedAt: start, Count: 30}, true, 0},
		{"a full window refuses until it ends", JoinWindow{StartedAt: start, Count: 30}, start.Add(20 * time.Second), 30,
			JoinWindow{StartedAt: start, Count: 30}, false, 40 * time.Second},
		{"the window's end opens a new one", JoinWindow{StartedAt: start, Count: 30}, start.Add(time.Minute), 30,
			JoinWindow{StartedAt: start.Add(time.Minute), Count: 1}, true, 0},
		{"long after the window", JoinWindow{StartedAt: start, Count: 30}, start.Add(time.Hour), 30,
			JoinWindow{StartedAt: start.Add(time.Hour), Count: 1}, true, 0},
		{"a clock behind the window's start counts in it", JoinWindow{StartedAt: start, Count: 30}, start.Add(-5 * time.Second), 30,
			JoinWindow{StartedAt: start, Count: 30}, false, time.Minute},
		{"a clock behind the start still admits below the limit", JoinWindow{StartedAt: start, Count: 2}, start.Add(-5 * time.Second), 30,
			JoinWindow{StartedAt: start, Count: 3}, true, 0},
		{"no limit admits nothing", JoinWindow{}, start, 0,
			JoinWindow{StartedAt: start}, false, time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, ok, retry := CountJoinAttempt(c.w, c.now, c.limit, JoinWindowLength)
			if !next.StartedAt.Equal(c.next.StartedAt) || next.Count != c.next.Count || ok != c.ok || retry != c.retry {
				t.Fatalf("got %+v ok=%v retry=%v, want %+v ok=%v retry=%v", next, ok, retry, c.next, c.ok, c.retry)
			}
		})
	}
}

// The thirty-first attempt of a minute is the first refusal, whatever the
// first thirty were.
func TestCountJoinAttemptRefusesTheThirtyFirst(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w := JoinWindow{}
	for i := 1; i <= 30; i++ {
		next, ok, _ := CountJoinAttempt(w, start.Add(time.Duration(i)*time.Second), 30, JoinWindowLength)
		if !ok {
			t.Fatalf("attempt %d was refused", i)
		}
		w = next
	}
	if _, ok, retry := CountJoinAttempt(w, start.Add(31*time.Second), 30, JoinWindowLength); ok || retry <= 0 || retry > JoinWindowLength {
		t.Fatalf("the thirty-first attempt answered ok=%v retry=%v", ok, retry)
	}
}
