package calls

import (
	"strings"
	"testing"

	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestCallTitlesCountGraphemesAndBoundBytes(t *testing.T) {
	for _, input := range []string{strings.Repeat("e\u0301", 80), strings.Repeat("👨‍👩‍👧‍👦", 80), "<script>alert(1)</script>", ""} {
		if _, err := checkTitle(input); err != nil {
			t.Errorf("valid title refused: %v", err)
		}
	}
	for _, input := range []string{strings.Repeat("e\u0301", 81), "a" + strings.Repeat("\u0301", 1024)} {
		if _, err := checkTitle(input); callrules.ReasonOf(err) != "title_too_long" {
			t.Errorf("unbounded title accepted: %v", err)
		}
	}
	if _, err := checkTitle(string([]byte{0xff})); callrules.ReasonOf(err) != "title_invalid" {
		t.Errorf("invalid UTF-8 accepted: %v", err)
	}
	if got, err := checkTitle("  Team\r\n\x00 review\t "); err != nil || got != "Team review" {
		t.Errorf("one-line title = %q, %v", got, err)
	}
}
