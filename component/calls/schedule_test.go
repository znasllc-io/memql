package calls

import (
	"errors"
	"testing"
	"time"
)

func TestValidateSchedule(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p := DefaultPolicy()
	iv := func(startMin, lenMin int) Interval {
		s := now.Add(time.Duration(startMin) * time.Minute)
		return Interval{Start: s, End: s.Add(time.Duration(lenMin) * time.Minute)}
	}
	many := make([]Interval, 20)
	for i := range many {
		many[i] = iv(1000+i*100, 30)
	}
	cases := []struct {
		name     string
		want     Interval
		upcoming []Interval
		err      error
	}{
		{"ok", iv(60, 60), nil, nil},
		{"exactly 480 minutes", iv(60, 480), nil, nil},
		{"past", iv(-5, 30), nil, ErrInPast},
		{"beyond horizon", Interval{Start: now.Add(31 * 24 * time.Hour), End: now.Add(31*24*time.Hour + time.Hour)}, nil, ErrBeyondHorizon},
		{"too long", iv(60, 481), nil, ErrTooLong},
		{"overlap", iv(60, 60), []Interval{iv(90, 60)}, ErrOverlaps},
		{"touching is fine", iv(60, 60), []Interval{iv(120, 60)}, nil},
		{"21st upcoming", iv(30000, 30), many, ErrTooManyUpcoming},
		{"empty interval", Interval{Start: now.Add(time.Hour), End: now.Add(time.Hour)}, nil, ErrTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateSchedule(now, c.want, p, c.upcoming); !errors.Is(err, c.err) {
				t.Fatalf("got %v want %v", err, c.err)
			}
		})
	}
}
