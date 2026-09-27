package emailrules

import (
	"testing"
	"time"
)

// TestFiredAtAfterPlacesTheRecordAfterTheVersionItRead: a firing is stamped at
// the local clock unless the version it read is not in the past, and then one
// microsecond after that version -- compared at the microsecond the column
// stores, so a sub-microsecond lead cannot round onto the prior's key.
func TestFiredAtAfterPlacesTheRecordAfterTheVersionItRead(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 500_000_600, time.UTC) // .500000600
	us := time.Microsecond
	nowUs := now.Truncate(us)
	for _, c := range []struct {
		name  string
		prior time.Time
		want  time.Time
	}{
		{"no prior version read", time.Time{}, nowUs},
		{"a prior version in the past", nowUs.Add(-time.Second), nowUs},
		{"a prior version one microsecond back", nowUs.Add(-us), nowUs},
		{"a prior version at this very microsecond", nowUs, nowUs.Add(us)},
		{"a prior version less than a microsecond back", now.Add(-300 * time.Nanosecond), nowUs.Add(us)},
		{"a prior version stamped by a clock that runs ahead", nowUs.Add(30 * time.Second), nowUs.Add(30*time.Second + us)},
		{"a prior version in another zone", nowUs.Add(time.Minute).In(time.FixedZone("x", 3600)), nowUs.Add(time.Minute + us)},
	} {
		got := firedAtAfter(c.prior, now)
		if !got.Equal(c.want) {
			t.Errorf("%s: firedAtAfter(%s) = %s, want %s", c.name, c.prior.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano), c.want.Format(time.RFC3339Nano))
		}
		if !c.prior.IsZero() && !got.After(c.prior) {
			t.Errorf("%s: %s is not after the version it was derived from (%s)", c.name, got.Format(time.RFC3339Nano), c.prior.Format(time.RFC3339Nano))
		}
		if got.Location() != time.UTC || got.Nanosecond()%1000 != 0 {
			t.Errorf("%s: %s is not a UTC microsecond", c.name, got.Format(time.RFC3339Nano))
		}
	}
}

// TestFiredAtLayoutIsTheColumnsPrecision: the text written is the instant
// compared -- six fractional digits, never trimmed, never more.
func TestFiredAtLayoutIsTheColumnsPrecision(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 120_000, time.UTC)
	if got, want := at.Format(firedAtLayout), "2026-09-27T12:00:00.000120Z"; got != want {
		t.Fatalf("firedAtLayout renders %q, want %q", got, want)
	}
	back, err := time.Parse(time.RFC3339Nano, at.Format(firedAtLayout))
	if err != nil || !back.Equal(at) {
		t.Fatalf("the rendered time does not parse back to itself: %v, %v", back, err)
	}
}
