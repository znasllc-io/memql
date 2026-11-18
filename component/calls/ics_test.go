package calls

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func goldenEvent() CalendarEvent {
	zone := time.FixedZone("x", 2*3600)
	return CalendarEvent{
		UID:     "call-1;a,b@example.invalid",
		Summary: "Planning, part 1; agenda\\notes",
		Description: "First line.\nSecond line, with a comma; a semicolon; a backslash \\ and a long tail " +
			"that must fold across several lines, including a multi-byte character right here: café " +
			"éééééééééééééééééééé end.",
		URL:       "https://calls.example.invalid/join?room=abc&x=1,2;3",
		ProductID: "-//Example//Calls//EN",
		Start:     time.Date(2026, 12, 1, 15, 30, 0, 0, zone),
		End:       time.Date(2026, 12, 1, 16, 30, 0, 0, zone),
		Stamp:     time.Date(2026, 11, 17, 9, 0, 5, 0, time.UTC),
	}
}

func TestRenderICSMatchesGolden(t *testing.T) {
	got := RenderICS(goldenEvent())
	want, err := os.ReadFile("testdata/scheduled.ics")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ICS differs from testdata/scheduled.ics.\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderICSRules(t *testing.T) {
	out := string(RenderICS(goldenEvent()))
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatal("every line break must be CRLF")
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("line over 75 octets (%d): %q", len(line), line)
		}
	}
	for _, must := range []string{"METHOD:PUBLISH", "DTSTART:20261201T133000Z", "DTEND:20261201T143000Z", "DTSTAMP:20261117T090005Z"} {
		if !strings.Contains(out, must) {
			t.Fatalf("missing %q", must)
		}
	}
	if strings.Contains(out, "ATTENDEE") {
		t.Fatal("no ATTENDEE line")
	}
	unfolded := strings.ReplaceAll(out, "\r\n ", "")
	if !strings.Contains(unfolded, "URL:https://calls.example.invalid/join?room=abc&x=1,2;3\r\n") {
		t.Fatal("the URL must not be escaped")
	}
	if !strings.Contains(unfolded, `SUMMARY:Planning\, part 1\; agenda\\notes`) {
		t.Fatal("SUMMARY must escape comma, semicolon and backslash")
	}
}
