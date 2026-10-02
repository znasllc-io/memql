package campaigns

import (
	"testing"
	"time"
)

func TestRecurringCalendarCadenceAndDowntime(t *testing.T) {
	zone := "America/New_York"
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2027, 3, 1, 9, 0, 0, 0, location)
	for _, weeks := range []int{2, 3} {
		next, err := nextSeriesTime(anchor, weeks, zone, anchor)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2027, 3, 1+7*weeks, 9, 0, 0, 0, location)
		if !next.Equal(want) || next.In(location).Hour() != 9 {
			t.Fatalf("%d week DST cadence: %s", weeks, next)
		}
		after := anchor.AddDate(0, 0, 7*weeks*5+3)
		next, err = nextSeriesTime(anchor, weeks, zone, after)
		if err != nil || !next.Equal(anchor.AddDate(0, 0, 7*weeks*6)) {
			t.Fatalf("catch-up must skip missed slots: %s %v", next, err)
		}
	}
	// A nonexistent spring-forward time normalizes for that occurrence only.
	gapAnchor := time.Date(2027, 3, 7, 2, 30, 0, 0, location)
	next, err := nextSeriesTime(gapAnchor, 1, zone, time.Date(2027, 3, 15, 0, 0, 0, 0, location))
	if err != nil || next.In(location).Day() != 21 || next.In(location).Hour() != 2 {
		t.Fatalf("gap changed later calendar times: %s %v", next, err)
	}
	if _, err = nextSeriesTime(anchor, 0, zone, anchor); err == nil {
		t.Fatal("invalid cadence admitted")
	}
	if _, err = nextSeriesTime(anchor, 2, "Local", anchor); err == nil {
		t.Fatal("machine-dependent timezone admitted")
	}
	if occurrenceID("series", anchor) == occurrenceID("series", anchor.AddDate(0, 0, 14)) {
		t.Fatal("different occurrences share a ledger")
	}
}
