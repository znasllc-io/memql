package crash

import (
	"strings"
	"testing"
)

// TestSanitizeForCrashLog_StripsKnownTokens walks each redaction pattern
// against a representative sample. The central invariant: the original
// token never survives the sanitizer; some recognisable placeholder
// does.
func TestSanitizeForCrashLog_StripsKnownTokens(t *testing.T) {
	cases := []struct {
		name           string
		input          string
		mustNotContain string
		mustContain    string
	}{
		{
			name:           "PAT",
			input:          "/* in frame: token=mql_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ */",
			mustNotContain: "mql_pat_abcdefghij",
			mustContain:    "mql_pat_<REDACTED>",
		},
		{
			name:           "worker token",
			input:          "auth header was mql_wkr_AbCdEfGh-_1234567890ABCDEFGHIJ0987654321XYZWVU",
			mustNotContain: "AbCdEfGh-_",
			mustContain:    "mql_wkr_<REDACTED>",
		},
		{
			name:           "voice-agent token",
			input:          "loaded mql_va_secret123long_enough_to_match from env",
			mustNotContain: "secret123long_enough",
			mustContain:    "mql_va_<REDACTED>",
		},
		{
			name:           "JWT",
			input:          "Authorization=eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.signature_data_here",
			mustNotContain: "eyJhbGciOiJSUzI1NiJ9",
			mustContain:    "<JWT-REDACTED>",
		},
		{
			name:           "Authorization Bearer header",
			input:          "metadata: Authorization: Bearer eyJhdGVzdC50b2tlbi52YWx1ZQ==",
			mustNotContain: "eyJhdGVzdC50b2tlbi52YWx1ZQ",
			mustContain:    "Authorization: Bearer <REDACTED>",
		},
		{
			name:           "Authorization Pair header",
			input:          "request had `Authorization: Pair AAAA-BBBB-CCCC-DDDD` set",
			mustNotContain: "AAAA-BBBB-CCCC-DDDD",
			mustContain:    "Authorization: Pair <REDACTED>",
		},
		{
			name:           "Authorization Worker header",
			input:          "Authorization: Worker mql_wkr_super_secret_token_value",
			mustNotContain: "super_secret_token_value",
			// Either the PAT/wkr/va redaction OR the Authorization Worker
			// redaction (or both) might fire; just assert SOMETHING got
			// scrubbed.
			mustContain: "REDACTED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeForCrashLog(tc.input)
			if tc.mustNotContain != "" && strings.Contains(got, tc.mustNotContain) {
				t.Errorf("sanitizer left raw token:\n  input  = %q\n  output = %q\n  found  = %q",
					tc.input, got, tc.mustNotContain)
			}
			if tc.mustContain != "" && !strings.Contains(got, tc.mustContain) {
				t.Errorf("sanitizer dropped placeholder:\n  input    = %q\n  output   = %q\n  expected = %q",
					tc.input, got, tc.mustContain)
			}
		})
	}
}

// TestSanitizeForCrashLog_PreservesNonTokenText asserts the sanitizer
// doesn't aggressively overwrite normal stack-trace content. Frame
// names, file paths, line numbers, ordinary identifiers should pass
// through unchanged.
func TestSanitizeForCrashLog_PreservesNonTokenText(t *testing.T) {
	input := `goroutine 17 [running]:
runtime/debug.Stack()
	/usr/local/go/src/runtime/debug/stack.go:24 +0x65
github.com/znasllc-io/memql/cockpit/internal/crash.newReport({0xabcdef, 0x7}, {0x123, 0x4})
	/Users/dev/cli/crash/crash.go:88 +0x88
main.main()
	/Users/dev/cmd/memql/main.go:42 +0xff`

	got := SanitizeForCrashLog(input)
	for _, want := range []string{
		"goroutine 17 [running]",
		"runtime/debug.Stack",
		"crash.newReport",
		"main.go:42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitizer over-redacted: lost %q in output\n%s", want, got)
		}
	}
}

// TestSanitizeForCrashLog_IsIdempotent — running the sanitizer twice
// produces the same result. (Failure here would mean a pattern
// re-matches its own placeholder, which would be a bug.)
func TestSanitizeForCrashLog_IsIdempotent(t *testing.T) {
	input := "mql_pat_abcdefghij0123456789ABCDEFGH_long enough JWT eyJabc.def.ghi"
	once := SanitizeForCrashLog(input)
	twice := SanitizeForCrashLog(once)
	if once != twice {
		t.Errorf("sanitizer not idempotent:\n  once  = %q\n  twice = %q", once, twice)
	}
}
