package abuse

import (
	"math"
	"strings"
)

// ScoreInput captures the per-request signals the scorer evaluates.
// All fields are optional — zero values are treated as "no contribution
// to the score." Callers populate as much as they have.
type ScoreInput struct {
	// Email is the address the user supplied; lowercased before
	// scoring is helpful but not required.
	Email string

	// SourceIP is the originating IP, used today only for telemetry
	// (the rate limiter owns the per-IP velocity bucket directly).
	SourceIP string

	// UserAgent is the User-Agent header, scanned for automation
	// keywords (curl, wget, headless, bot, phantomjs, etc.).
	UserAgent string

	// DisposableHit signals the email's domain matched the embedded
	// blocklist. Carried through here so the score reflects
	// disposable-email contribution even when the explicit
	// IsDisposable check did not bounce the request (e.g., when the
	// blocklist is informational rather than blocking).
	DisposableHit bool

	// MXFailed signals the DNS lookup found no MX or A/AAAA records.
	MXFailed bool

	// TurnstileBorderline reports Turnstile returned challenged but
	// not failed — legit-looking but flagged. Hard failures bounce
	// before scoring runs.
	TurnstileBorderline bool

	// IPVelocity is the count of recent submissions from SourceIP in
	// the last hour, as observed by the per-IP rate limiter. Bucket
	// boundaries (medium/high) are encoded in Score; callers pass the
	// raw count.
	IPVelocity int

	// HourOfDay is the request hour in UTC (0-23). The 3-6am UTC band
	// catches a chunk of automated registration storms; legitimate
	// signups skew toward business hours in any timezone.
	HourOfDay int
}

// ScoreResult is the scorer's output. Score is "higher is worse";
// Signals lists every contributing reason for traceability in the
// audit event.
type ScoreResult struct {
	Score   int
	Signals []string
}

// Weight constants are exported so tests and admin-UI tooling can
// reason about them. Calibrated so IDENTITY_RISK_THRESHOLD=50
// (the default) sits at the "block on first strong signal OR two
// medium signals" line.
const (
	WeightDisposableHit       = 30
	WeightMXFail              = 30
	WeightTurnstileBorderline = 15
	WeightIPVelocityHigh      = 20 // >=3 recent submissions from same IP
	WeightIPVelocityMedium    = 5  // >=1 but < 3 recent submissions
	WeightTimeOfDayOdd        = 10 // 3-6am UTC
	WeightUAAutomation        = 25 // automation/bot User-Agent keyword
	WeightEmailRandomLooking  = 10 // local-part looks generated
)

// Score computes the abuse-risk score for a magic-link request.
// Higher is worse. Signals lists every contributor for the audit
// trail. The function is pure — repeatable inputs produce repeatable
// outputs.
func Score(in ScoreInput) ScoreResult {
	var (
		score   int
		signals []string
	)

	if in.DisposableHit {
		score += WeightDisposableHit
		signals = append(signals, "disposable_hit")
	}
	if in.MXFailed {
		score += WeightMXFail
		signals = append(signals, "mx_fail")
	}
	if in.TurnstileBorderline {
		score += WeightTurnstileBorderline
		signals = append(signals, "turnstile_borderline")
	}

	switch {
	case in.IPVelocity >= 3:
		score += WeightIPVelocityHigh
		signals = append(signals, "ip_velocity_high")
	case in.IPVelocity >= 1:
		score += WeightIPVelocityMedium
		signals = append(signals, "ip_velocity_medium")
	}

	if in.HourOfDay >= 3 && in.HourOfDay < 6 {
		score += WeightTimeOfDayOdd
		signals = append(signals, "time_of_day_3_to_6_utc")
	}

	if isAutomationUA(in.UserAgent) {
		score += WeightUAAutomation
		signals = append(signals, "ua_automation_keyword")
	}

	if isRandomLookingLocalPart(in.Email) {
		score += WeightEmailRandomLooking
		signals = append(signals, "email_local_part_random_looking")
	}

	return ScoreResult{Score: score, Signals: signals}
}

// automationKeywords are substrings that strongly suggest a non-browser
// User-Agent. The list deliberately stays short and case-insensitive —
// false positives here block legitimate scripted clients (which is
// usually what we want on a public registration form anyway).
var automationKeywords = []string{
	"curl/",
	"wget/",
	"python-requests/",
	"go-http-client",
	"java/",
	"libwww-perl",
	"httpie",
	"axios/",
	"node-fetch",
	"bot",
	"headlesschrome",
	"headless",
	"phantomjs",
	"selenium",
	"puppeteer",
	"playwright",
}

// isAutomationUA returns true when ua matches a known automation
// keyword (case-insensitive substring match).
func isAutomationUA(ua string) bool {
	if ua == "" {
		// Empty UA is itself suspicious but not a hard signal —
		// browsers always send one. Score it as automation.
		return true
	}
	lower := strings.ToLower(ua)
	for _, kw := range automationKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isRandomLookingLocalPart applies a small heuristic to the email's
// local-part: long + heavy on digits is the canonical
// auto-generated signature. We deliberately keep this simple — the
// false-positive cost on legitimate-but-numeric usernames (e.g.,
// "user12345@…") is low because this only contributes 10 to the
// score, well below the 50 default threshold by itself.
func isRandomLookingLocalPart(email string) bool {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return false
	}
	local := email[:at]
	if len(local) < 12 {
		return false
	}
	digitCount := 0
	for i := 0; i < len(local); i++ {
		if local[i] >= '0' && local[i] <= '9' {
			digitCount++
		}
	}
	digitFraction := float64(digitCount) / float64(len(local))
	if digitFraction >= 0.30 {
		return true
	}
	// Secondary check: high entropy on the alpha portion. Anything
	// above 3.5 bits/char in the alpha-only stream is a stronger
	// signal than the digit-fraction rule and catches alpha-heavy
	// random strings ("abxqwrnpfg…").
	if shannonEntropy(local) >= 3.5 && len(local) >= 16 {
		return true
	}
	return false
}

// shannonEntropy computes the per-character Shannon entropy of s in
// bits. Returns 0 for empty input.
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	counts := make(map[rune]int, len(s))
	for _, r := range s {
		counts[r]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
