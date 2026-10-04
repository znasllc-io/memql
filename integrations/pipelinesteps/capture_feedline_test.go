package pipelinesteps

import (
	"strings"
	"testing"
	"time"
)

// capture_feedline_test.go -- the unstamped entry point a fleet machine's
// output goes through (epic memql#5478, #5494). Kept out of capture_test.go so
// the runner's additions to that file and this one never meet.

// TestCaptureFeedLineKeepsALeadingTimestampAsOutput: a fleet machine's stream
// carries no kubelet timestamp, so a line that BEGINS with one is the step's
// own text. Feed would take that token for the kubelet's stamp and eat it --
// and let the step choose the line's time; FeedLine keeps the whole line and
// stamps it with the capture's clock.
func TestCaptureFeedLineKeepsALeadingTimestampAsOutput(t *testing.T) {
	const secret = "fleet-s3cr3t-value"
	c, sink := newCaptureForTest(t, CaptureOptions{Secrets: []string{secret}})

	stamped := "2026-01-01T00:00:00Z the step printed a timestamp first"
	if at := c.FeedLine(stamped); !at.Equal(captureTestClock) {
		t.Errorf("FeedLine stamped the line %v, want the capture's clock %v: the step's text chose its own time", at, captureTestClock)
	}
	c.FeedLine("a line with " + secret + " in it\r")
	c.FeedLine("two\nlines\n")
	res := closeCaptureForTest(t, c)
	c.FeedLine("after the close")

	want := []string{stamped, "a line with *** in it", "two", "lines"}
	if got := strings.Split(strings.TrimSuffix(readArchiveForTest(t, res), "\n"), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("archive = %q, want %q", got, want)
	}
	if res.Lines != 4 {
		t.Errorf("Lines = %d, want 4", res.Lines)
	}
	msgs := sink.messages()
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Errorf("store = %q, want %q", msgs, want)
	}
	for _, l := range sink.all() {
		if !l.At.Equal(captureTestClock) {
			t.Errorf("store line %q at %v, want the capture's clock", l.Message, l.At)
		}
	}
	if !strings.Contains(res.Tail, stamped) || strings.Contains(res.Tail, secret) {
		t.Errorf("tail = %q, want the whole stamped line and no secret", res.Tail)
	}

	// THE CONTRAST that makes FeedLine necessary: the same text through Feed
	// loses its first token to the timestamp sniff.
	k, _ := newCaptureForTest(t, CaptureOptions{})
	if at := k.Feed(stamped); at.Equal(captureTestClock) || at.Year() != 2026 || at.Month() != time.January {
		t.Fatalf("Feed took %v from the line; the contrast assumes it reads the leading token as the kubelet's stamp", at)
	}
	if archive := readArchiveForTest(t, closeCaptureForTest(t, k)); strings.Contains(archive, "2026-01-01") {
		t.Fatalf("Feed kept the leading token (%q); FeedLine would not be needed", archive)
	}
}
