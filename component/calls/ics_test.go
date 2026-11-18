package calls

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestRenderICSStructureIsIndependentOfTheGolden checks the rendered bytes
// against rules and a hand-written list of logical lines, so it still fails
// if the golden file were regenerated from a wrong renderer.
func TestRenderICSStructureIsIndependentOfTheGolden(t *testing.T) {
	out := string(RenderICS(goldenEvent()))
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatal("the file must end with CRLF")
	}
	bare := strings.ReplaceAll(out, "\r\n", "")
	if strings.Contains(bare, "\r") || strings.Contains(bare, "\n") {
		t.Fatal("a bare CR or LF occurs")
	}
	physical := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
	for i, l := range physical {
		if len(l) > 75 {
			t.Fatalf("physical line %d is %d octets", i, len(l))
		}
		if !utf8.ValidString(l) {
			t.Fatalf("physical line %d splits a UTF-8 sequence: %q", i, l)
		}
		if strings.HasPrefix(l, "ATTENDEE") {
			t.Fatal("no ATTENDEE line")
		}
	}

	want := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//Example//Calls//EN",
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		`UID:call-1\;a\,b@example.invalid`,
		"DTSTAMP:20261117T090005Z",
		"DTSTART:20261201T133000Z",
		"DTEND:20261201T143000Z",
		`SUMMARY:Planning\, part 1\; agenda\\notes`,
		`DESCRIPTION:First line.\nSecond line\, with a comma\; a semicolon\; a backslash \\ and a long tail ` +
			`that must fold across several lines\, including a multi-byte character right here: caf` + "é " +
			strings.Repeat("é", 20) + " end.",
		"URL:https://calls.example.invalid/join?room=abc&x=1,2;3",
		"END:VEVENT",
		"END:VCALENDAR",
	}
	got := strings.Split(strings.TrimSuffix(strings.ReplaceAll(out, "\r\n ", ""), "\r\n"), "\r\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unfolded lines differ.\n got: %q\nwant: %q", got, want)
	}
}

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
