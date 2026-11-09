package campaigns

import (
	"strings"
	"testing"
	"time"
)

// TestJourneyRequestIDReadsBackToItsEnrollment holds JourneyEnrollmentOfRequest
// to the writer it reads: every request id a journey step sends under names
// its enrollment, whatever the step is called, and the email statistics read
// nothing else as a journey send (D18 of the 2026-11-08 statistics design).
func TestJourneyRequestIDReadsBackToItsEnrollment(t *testing.T) {
	enrollment := journeyEnrollmentID("journey-1", "recipient-1", time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC))
	for _, step := range []string{"welcome", "step-2", "a", "Confirm_1", strings.Repeat("s", 40)} {
		if !journeyStepIDPattern.MatchString(step) {
			t.Fatalf("the fixture step %q is not a step id", step)
		}
		got, ok := JourneyEnrollmentOfRequest(journeyRequestID(enrollment, step))
		if !ok || got != enrollment {
			t.Errorf("the request for step %q read back as %q, %v; want %q", step, got, ok, enrollment)
		}
	}
	// The writer bares a canonical enrollment id; the reader answers the bare one.
	if got, ok := JourneyEnrollmentOfRequest(journeyRequestID("v1:campaigns:journeyEnrollment:"+enrollment, "welcome")); !ok || got != enrollment {
		t.Errorf("a request written from a canonical enrollment id read back as %q, %v; want %q", got, ok, enrollment)
	}

	for _, requestID := range []string{
		"",
		"journey-",
		"journey-short-welcome",
		"newsletter-" + enrollment,
		"journey-" + enrollment,
		"journey-" + enrollment + "-",
		"journey-" + strings.ToUpper(enrollment) + "-welcome",
		"journey-" + enrollment + "-" + strings.Repeat("s", 41),
	} {
		if got, ok := JourneyEnrollmentOfRequest(requestID); ok {
			t.Errorf("%q read as the enrollment %q, want no journey", requestID, got)
		}
	}
}
