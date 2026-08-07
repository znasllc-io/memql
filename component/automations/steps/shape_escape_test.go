package steps

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// shape_escape_test.go -- memql#3190, the shape-data parser's four scans.
//
// parseShapeValue decided where a literal ended with a ONE-BYTE LOOKBACK
// (`s[end-1] != '\\'`), in four places: the quoted-string arm and the paren /
// brace / bracket depth scanners. A lookback cannot tell an escaped quote from
// a quote that follows a COMPLETED `\\` escape, so a literal ending in a
// backslash pair read its own closing quote as escaped, stayed in string
// state, and consumed on to the next quote -- swallowing the `,` / `}` / `]`
// that ended the value and mis-parsing every field after it.
//
// This parser accepts BOTH `"` and `'`, so every case below is run under both
// quote characters: the single-quoted arm is the reason these scans track
// escape state locally instead of delegating to a shared `"`-only blanker.

func evalForShapeTest() *automations.Evaluator {
	return automations.NewEvaluator()
}

func TestParseShapeValue_StringEndingInCompletedEscape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		wantValue string
		wantRest  string
	}{
		{
			name:      "double-quoted literal ending in a completed backslash escape",
			in:        `"C:\\", "next": 1`,
			wantValue: `C:\\`,
			wantRest:  `, "next": 1`,
		},
		{
			name:      "single-quoted literal ending in a completed backslash escape",
			in:        `'C:\\', 'next': 1`,
			wantValue: `C:\\`,
			wantRest:  `, 'next': 1`,
		},
		{
			name:      "escaped quote still does not end the literal",
			in:        `"he said \"hi\"", rest`,
			wantValue: `he said \"hi\"`,
			wantRest:  `, rest`,
		},
		{
			name:      "plain literal is unaffected",
			in:        `"plain", rest`,
			wantValue: `plain`,
			wantRest:  `, rest`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := parseShapeValue(tc.in, evalForShapeTest())
			if err != nil {
				t.Fatalf("parseShapeValue(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.wantValue {
				t.Errorf("parseShapeValue(%q) value = %q, want %q", tc.in, got, tc.wantValue)
			}
			if rest != tc.wantRest {
				t.Errorf("parseShapeValue(%q) remaining = %q, want %q", tc.in, rest, tc.wantRest)
			}
		})
	}
}

// The three depth scanners (paren / brace / bracket) carried the identical
// lookback and now share one local helper, so each is driven with a literal
// ending in a completed `\\` escape.
func TestParseShapeValue_DepthScannersSurviveCompletedEscape(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		wantRest string
		check    func(t *testing.T, got any)
	}{
		{
			name:     "nested object whose value ends in a completed escape",
			in:       `{"path": "C:\\"}, rest`,
			wantRest: `, rest`,
			check: func(t *testing.T, got any) {
				m, ok := got.(map[string]any)
				if !ok {
					t.Fatalf("value = %#v, want map", got)
				}
				if m["path"] != `C:\\` {
					t.Errorf("path = %#v, want %q", m["path"], `C:\\`)
				}
			},
		},
		{
			// Object KEYS are double-quoted by the object parser's own
			// grammar; the value is where both quote characters are accepted.
			name:     "single-quoted nested object value ending in a completed escape",
			in:       `{"path": 'C:\\'}, rest`,
			wantRest: `, rest`,
			check: func(t *testing.T, got any) {
				m, ok := got.(map[string]any)
				if !ok {
					t.Fatalf("value = %#v, want map", got)
				}
				if m["path"] != `C:\\` {
					t.Errorf("path = %#v, want %q", m["path"], `C:\\`)
				}
			},
		},
		{
			name:     "array whose element ends in a completed escape",
			in:       `["C:\\", "b"], rest`,
			wantRest: `, rest`,
			check: func(t *testing.T, got any) {
				arr, ok := got.([]any)
				if !ok {
					t.Fatalf("value = %#v, want slice", got)
				}
				if len(arr) != 2 || arr[0] != `C:\\` || arr[1] != "b" {
					t.Errorf("array = %#v, want [%q b]", arr, `C:\\`)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := parseShapeValue(tc.in, evalForShapeTest())
			if err != nil {
				t.Fatalf("parseShapeValue(%q): unexpected error %v", tc.in, err)
			}
			if rest != tc.wantRest {
				t.Errorf("parseShapeValue(%q) remaining = %q, want %q", tc.in, rest, tc.wantRest)
			}
			tc.check(t, got)
		})
	}
}

// The paren scanner is driven through the node() arm, whose argument resolves
// against the evaluator -- so the assertion is on the SCAN, not the resolved
// value: before the fix the call failed with "unmatched parenthesis" because
// the literal ending in `\\` swallowed the closing paren.
func TestParseShapeValue_ParenScannerSurvivesCompletedEscape(t *testing.T) {
	for _, in := range []string{
		`node("path\\"), rest`,
		`node('path\\'), rest`,
	} {
		_, _, err := parseShapeValue(in, evalForShapeTest())
		if err != nil && strings.Contains(err.Error(), "unmatched parenthesis") {
			t.Errorf("parseShapeValue(%q): %v -- the literal ending in `\\\\` swallowed the closing paren", in, err)
		}
	}
}

// The three depth scanners share one helper now; it is driven directly for
// each delimiter pair, including the single-quoted literal that is the reason
// this scan stays local rather than delegating to a `"`-only shared blanker.
func TestScanBalancedSpanEnd(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          string
		open, close byte
		want        int
	}{
		{name: "brace, literal ending in a completed escape", in: `{"a": "C:\\"} rest`, open: '{', close: '}', want: 13},
		{name: "brace, single-quoted literal ending in a completed escape", in: `{"a": 'C:\\'} rest`, open: '{', close: '}', want: 13},
		{name: "brace, escaped quote holds the literal open", in: `{"a": "x\"}"} rest`, open: '{', close: '}', want: 13},
		{name: "bracket, literal ending in a completed escape", in: `["C:\\"] rest`, open: '[', close: ']', want: 8},
		{name: "paren, literal ending in a completed escape", in: `("C:\\") rest`, open: '(', close: ')', want: 8},
		{name: "nested depth", in: `{"a": {"b": 1}} rest`, open: '{', close: '}', want: 15},
		{name: "delimiter inside a literal is not counted", in: `{"a": "}"} rest`, open: '{', close: '}', want: 10},
		{name: "unbalanced", in: `{"a": 1`, open: '{', close: '}', want: -1},
		{name: "unterminated literal", in: `{"a": "x}`, open: '{', close: '}', want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanBalancedSpanEnd(tc.in, tc.open, tc.close); got != tc.want {
				t.Errorf("scanBalancedSpanEnd(%q, %q, %q) = %d, want %d", tc.in, tc.open, tc.close, got, tc.want)
			}
		})
	}
}

// The existing contract must survive the fix: an unterminated literal and an
// unbalanced delimiter are still errors.
func TestParseShapeValue_PreservesExistingContract(t *testing.T) {
	if _, _, err := parseShapeValue(`"unterminated`, evalForShapeTest()); err == nil {
		t.Error("unterminated string must still be an error")
	}
	if _, _, err := parseShapeValue(`{"a": 1`, evalForShapeTest()); err == nil {
		t.Error("unmatched brace must still be an error")
	}
	if _, _, err := parseShapeValue(`["a"`, evalForShapeTest()); err == nil {
		t.Error("unmatched bracket must still be an error")
	}
}
