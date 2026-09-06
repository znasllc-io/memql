package dsl_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func calendarDSL(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	p := filepath.Join(filepath.Dir(file), "calendar", name)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCalendarBookingIsCoreNotAPack(t *testing.T) {
	src := calendarDSL(t, "concepts.memql")
	if !strings.Contains(src, "concept bookingHours") || !strings.Contains(src, "concept booking") {
		t.Fatal("bookingHours and booking must live in dsl/calendar (core)")
	}
	if strings.Contains(strings.ToLower(src), "calendly.com") {
		t.Fatal("no Calendly dependency")
	}
}

func TestCalendarBookingWritePath(t *testing.T) {
	mut := calendarDSL(t, "mutations.memql")
	for _, name := range []string{
		"mutate bookingHours createBookingHours",
		"mutate bookingHours setBookingHours",
		"mutate booking takeBooking",
		"mutate booking cancelBooking",
		"mutate booking rescheduleBooking",
	} {
		if !strings.Contains(mut, name) {
			t.Fatalf("missing %s", name)
		}
	}
}

// The three reads a booking surface needs, whatever renders it.
//
// IT USED TO ASSERT A SECOND THING and no longer can: that
// dsl/portalviews/concepts.memql cited memql#4142, which was how the repo
// recorded "booking is served by the arrangement system rather than by a new
// clients/ app". Epic memql#4984 deleted that domain with the portal, so the
// claim has no file to live in -- and inventing a new home for it would be a
// gate asserting over a decision nobody is at risk of reversing, since
// clients/ is allowlisted (clients_allowlist_test.go) and adding an app there
// is already a reviewable act.
func TestCalendarBookingQueries(t *testing.T) {
	q := calendarDSL(t, "queries.memql")
	for _, name := range []string{
		"query bookingHours bookingHours",
		"query booking hostedBookings",
		"query booking bookingById",
	} {
		if !strings.Contains(q, name) {
			t.Fatalf("missing %s", name)
		}
	}
}

func mustCaller(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("no caller")
	}
	return file
}
