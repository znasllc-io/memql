package calls

import (
	"errors"
	"testing"
	"time"
)

func TestPassAllowanceCarriesAcrossUTCMidnightWithoutResettingTotal(t *testing.T) {
	start := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	pass := AccountPass{ID: "pass", ActivatedAt: start.Add(-time.Hour), ExpiresAt: start.AddDate(0, 0, 30), TotalSeconds: 20 * 3600, DailySeconds: 3 * 3600, MaxPeople: 8}
	got, err := PassAllowance(pass, nil, start)
	if err != nil || !got.AvailableUntil.Equal(start.Add(4*time.Hour)) || !got.DailyResetsAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("midnight continuation: %+v, %v", got, err)
	}
	pass.TotalSeconds = 3 * 3600
	got, err = PassAllowance(pass, nil, start)
	if err != nil || !got.AvailableUntil.Equal(start.Add(3*time.Hour)) {
		t.Fatalf("midnight granted more than the pass total: %+v, %v", got, err)
	}
	// A partly used first day ends before midnight when no current-day
	// allowance remains to reach the reset. Future credit is not spendable now.
	pass.TotalSeconds = 20 * 3600
	pass.ActivatedAt = start.Add(-10 * time.Hour)
	rooms := []FundedRoomTime{{Start: start.Add(-3 * time.Hour), End: start.Add(-30 * time.Minute), Deadline: start}}
	got, err = PassAllowance(pass, rooms, start)
	if err != nil || got.DailyRemainingSeconds != 1800 || !got.AvailableUntil.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("crossed an exhausted day's gap: %+v, %v", got, err)
	}
}

func TestPassAllowanceUsesAdmissionAndAuthorizedEndsNotHistoryOrCleanup(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	pass := AccountPass{ID: "pass", ActivatedAt: now.Add(-24 * time.Hour), ExpiresAt: now.Add(24 * time.Hour), TotalSeconds: 20 * 3600, DailySeconds: 3 * 3600, MaxPeople: 8}
	rooms := []FundedRoomTime{
		{Deadline: now.Add(time.Hour)}, // queued or waiting, never admitted
		{Start: now.Add(-3 * time.Hour), End: now.Add(-time.Hour), Deadline: now.Add(-2 * time.Hour)},      // cleanup was late
		{Start: now.Add(-30 * time.Minute), End: now.Add(-20 * time.Minute), Deadline: now.Add(time.Hour)}, // ended early
		{Start: now.Add(-5 * time.Minute), Deadline: now.Add(time.Hour)},                                   // still running
	}
	got, err := PassAllowance(pass, rooms, now)
	if err != nil || got.RemainingSeconds != (20*3600-75*60) || got.DailyRemainingSeconds != 105*60 || !got.AvailableUntil.Equal(now.Add(105*time.Minute)) {
		t.Fatalf("wrong authoritative room time: %+v, %v", got, err)
	}
	// Prior-day time spends the total only. The daily bucket is UTC even if
	// the caller carries another location on its timestamp.
	rooms = []FundedRoomTime{{Start: now.Add(-13 * time.Hour), End: now.Add(-12 * time.Hour), Deadline: now.Add(-10 * time.Hour)}}
	got, err = PassAllowance(pass, rooms, now.In(time.FixedZone("west", -7*3600)))
	if err != nil || got.RemainingSeconds != 19*3600 || got.DailyRemainingSeconds != 3*3600 || got.DailyResetsAt.Location() != time.UTC {
		t.Fatalf("UTC bucket: %+v, %v", got, err)
	}
}

func TestPassAllowanceExpiryExhaustionAndInvalidStateFailClosed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	pass := AccountPass{ID: "pass", ActivatedAt: now.Add(-4 * time.Hour), ExpiresAt: now.Add(20 * time.Minute), TotalSeconds: 3600, DailySeconds: 10800, MaxPeople: 8}
	got, err := PassAllowance(pass, nil, now)
	if err != nil || !got.AvailableUntil.Equal(pass.ExpiresAt) {
		t.Fatalf("pass expiry was not the ceiling: %+v, %v", got, err)
	}
	got, err = PassAllowance(pass, nil, pass.ExpiresAt)
	if err != nil || got.RemainingSeconds != 0 || got.AvailableUntil.After(pass.ExpiresAt) {
		t.Fatalf("expired pass remained usable: %+v, %v", got, err)
	}
	rooms := []FundedRoomTime{{Start: now.Add(-time.Hour), End: now, Deadline: now.Add(time.Minute)}}
	got, err = PassAllowance(pass, rooms, now)
	if err != nil || got.RemainingSeconds != 0 || got.DailyRemainingSeconds != 7200 || got.AvailableUntil.After(now) {
		t.Fatalf("daily credit bypassed total exhaustion: %+v, %v", got, err)
	}
	pass.TotalSeconds = 1<<63 - 1
	if _, err = PassAllowance(pass, nil, now); !errors.Is(err, ErrAllowanceUnavailable) {
		t.Fatalf("overflowing stored limit was accepted: %v", err)
	}
	pass.TotalSeconds = 3600
	rooms[0].Deadline = pass.ExpiresAt.Add(time.Second)
	if _, err = PassAllowance(pass, rooms, now); !errors.Is(err, ErrAllowanceUnavailable) {
		t.Fatalf("room exceeded its pass: %v", err)
	}
}
