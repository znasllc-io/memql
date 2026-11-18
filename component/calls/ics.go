package calls

import (
	"strings"
	"time"
	"unicode/utf8"
)

// CalendarEvent is one scheduled call as a calendar file describes it.
type CalendarEvent struct {
	UID, Summary, Description, URL, ProductID string
	Start, End, Stamp                         time.Time
}

const (
	icsTimeLayout = "20060102T150405Z"
	icsMaxOctets  = 75
)

// icsText escapes a TEXT value (RFC 5545 3.3.11): backslash first so the
// escapes added after it are not doubled, then separators and newlines.
var icsText = strings.NewReplacer(
	"\\", "\\\\",
	";", "\\;",
	",", "\\,",
	"\r\n", "\\n",
	"\n", "\\n",
	"\r", "\\n",
)

// RenderICS writes one VEVENT as a METHOD:PUBLISH file with CRLF line ends.
// There is no ATTENDEE line: the file is a reminder with a join link, not an
// invitation that tracks replies. Times are UTC so no VTIMEZONE is needed.
// URL is a URI value and is not escaped.
func RenderICS(e CalendarEvent) []byte {
	var sb strings.Builder
	line := func(s string) {
		sb.WriteString(foldICS(s))
		sb.WriteString("\r\n")
	}
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:" + icsText.Replace(e.ProductID))
	line("CALSCALE:GREGORIAN")
	line("METHOD:PUBLISH")
	line("BEGIN:VEVENT")
	line("UID:" + icsText.Replace(e.UID))
	line("DTSTAMP:" + e.Stamp.UTC().Format(icsTimeLayout))
	line("DTSTART:" + e.Start.UTC().Format(icsTimeLayout))
	line("DTEND:" + e.End.UTC().Format(icsTimeLayout))
	line("SUMMARY:" + icsText.Replace(e.Summary))
	line("DESCRIPTION:" + icsText.Replace(e.Description))
	line("URL:" + e.URL)
	line("END:VEVENT")
	line("END:VCALENDAR")
	return []byte(sb.String())
}

// foldICS breaks a content line longer than 75 octets (CRLF excluded) by
// inserting CRLF and one space. The leading space counts toward the next
// line's 75, and a UTF-8 sequence is never split.
func foldICS(s string) string {
	if len(s) <= icsMaxOctets {
		return s
	}
	var sb strings.Builder
	limit := icsMaxOctets
	used := 0
	for _, r := range s {
		w := utf8.RuneLen(r)
		if used+w > limit {
			sb.WriteString("\r\n ")
			used = 1
		}
		sb.WriteRune(r)
		used += w
	}
	return sb.String()
}
