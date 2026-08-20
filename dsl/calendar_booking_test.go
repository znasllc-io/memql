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

func TestCalendarBookingPortalQueries(t *testing.T) {
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
	pv, err := os.ReadFile(filepath.Join(filepath.Dir(mustCaller(t)), "portalviews", "concepts.memql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pv), "memql#4142") {
		t.Fatal("portalviews must point at calendar booking rather than grow a new clients/ app")
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
