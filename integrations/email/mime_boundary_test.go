package email

import (
	"strings"
	"testing"
)

// The boundary is the only thing separating body content from MIME structure.
// A predictable one (it was time.Now().UnixNano() before memql#3348) turns
// "forge a MIME part" from impossible into a guessing problem.
func TestMIMEBoundaryIsUnpredictable(t *testing.T) {
	msg := Message{To: "a@example.com", Subject: "s", TextBody: "t", HTMLBody: "<p>h</p>"}

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		body, err := RenderRFC5322("sender@example.com", msg)
		if err != nil {
			t.Fatalf("RenderRFC5322: %v", err)
		}
		line := ""
		for _, l := range strings.Split(string(body), "\r\n") {
			if strings.HasPrefix(l, "Content-Type: multipart/alternative; boundary=") {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatal("no multipart boundary in the rendered message")
		}
		if seen[line] {
			t.Fatalf("boundary repeated across renders -- it is predictable: %s", line)
		}
		seen[line] = true
	}
	if len(seen) != 50 {
		t.Errorf("got %d distinct boundaries over 50 renders, want 50", len(seen))
	}
}
