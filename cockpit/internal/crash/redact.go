package crash

import (
	"regexp"
)

// SanitizeForCrashLog scrubs token-shaped strings from stack traces and
// panic values before they hit the crash log on disk. The log is
// mode 0600 and stays local, but support engineers often see it
// (the user is asked to paste it when filing a ticket), so the
// safest contents are the ones that never had a bearer token in
// them to begin with.
//
// The strategy is regex-based and deliberately conservative: every
// substitution preserves a stable, non-empty placeholder so the
// surrounding context (line numbers, frame names) stays readable.
// False positives (matching something that *looks* like a token but
// isn't) are acceptable — losing a stack frame value to <REDACTED>
// is strictly better than losing a real credential.
func SanitizeForCrashLog(s string) string {
	for _, p := range sanitizerPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

type sanitizerPattern struct {
	re   *regexp.Regexp
	repl string
}

var sanitizerPatterns = []sanitizerPattern{
	// mql_pat_<43 base64url> — PAT bearer.
	{re: regexp.MustCompile(`mql_pat_[A-Za-z0-9_\-]{20,}`), repl: "mql_pat_<REDACTED>"},
	// mql_wkr_<43 base64url> — worker token.
	{re: regexp.MustCompile(`mql_wkr_[A-Za-z0-9_\-]{20,}`), repl: "mql_wkr_<REDACTED>"},
	// mql_va_<...> — voice-agent shared secret.
	{re: regexp.MustCompile(`mql_va_[A-Za-z0-9_\-]{10,}`), repl: "mql_va_<REDACTED>"},
	// JWT (three dot-separated base64url segments). The leading
	// segment starts with `eyJ` (base64 of `{"`), which is the
	// distinctive prefix that lets us match JWTs without colliding
	// with arbitrary base64 strings elsewhere in the stack.
	{re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), repl: "<JWT-REDACTED>"},
	// "Authorization: Bearer <anything>" header dumps that some
	// frame might have stuffed into a local variable. Match
	// case-insensitively. The header value continues to the next
	// whitespace.
	{re: regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+\S+`), repl: "Authorization: Bearer <REDACTED>"},
	// "Authorization: Pair <code>" — pairing-code redeem path.
	{re: regexp.MustCompile(`(?i)Authorization:\s*Pair\s+\S+`), repl: "Authorization: Pair <REDACTED>"},
	// "Authorization: Worker <token>".
	{re: regexp.MustCompile(`(?i)Authorization:\s*Worker\s+\S+`), repl: "Authorization: Worker <REDACTED>"},
}
