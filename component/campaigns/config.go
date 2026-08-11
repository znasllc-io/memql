// Package campaigns is the email-campaign SENDING ENGINE (memql#3348):
// the worker that drains v1:campaigns:sendJob rows, the suppression and
// idempotency rules that decide who actually gets mailed, the RFC 8058
// one-click unsubscribe endpoint, and the DSL-callable surface an
// operator starts a send from.
//
// memql#3323 modelled campaigns as concepts and shipped the authoring UI,
// and deliberately stopped there: sending is a set of product decisions
// rather than UI. Those decisions are recorded where the code that
// implements each one lives --
//
//	suppression scope        dsl/campaigns/concepts.memql (v1:campaigns:suppression)
//	hard-bounce handling     capabilities.go (handleRecordFeedback)
//	idempotency              dsl/campaigns/concepts.memql (v1:campaigns:delivery) + worker.go
//	rate limits (ours)       limiter.go
//	rate limits (provider)   integrations/email/senderror.go + worker.go
//	unsubscribe              token.go + unsubscribe.go
//	SPF / DKIM alignment     render.go
//
// and summarised for operators in
// docs/public/operate/campaign-sending.md.
package campaigns

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/env"
)

const (
	// defaultPollSeconds is how often the drain loop looks for work.
	// Slower than the outbound worker's 15s because a campaign batch is
	// bounded by the send rate anyway -- polling faster would only find
	// an empty token bucket.
	defaultPollSeconds = 15

	// defaultStartupDelaySeconds defers the first drain past engine +
	// DSL load, matching the outbound worker's precedent (the engine
	// rejects queries mid-bootstrap).
	defaultStartupDelaySeconds = 25

	// defaultBatchSize bounds how many recipients one job gets per tick.
	// It exists so a 5000-address campaign cannot hold the loop while a
	// second campaign waits: every drainable job gets a batch each tick,
	// round-robin by age.
	defaultBatchSize = 25

	// defaultSendRatePerMinute is OUR ceiling, distinct from the
	// provider's. Microsoft Graph's per-mailbox limit is on the order of
	// 30 messages/minute before it starts returning 429, so the default
	// sits at that shape deliberately: the point of a self-imposed limit
	// is to stay under the provider's rather than to discover it. An
	// operator with a higher-throughput relay raises it.
	defaultSendRatePerMinute = 30

	// defaultMaxAttempts bounds per-recipient delivery attempts before
	// the delivery row goes terminal `failed`. Lower than the outbound
	// worker's 5: a marketing message has a shelf life, and the eighth
	// retry of a soft-bouncing address costs reputation rather than
	// buying delivery.
	defaultMaxAttempts = 3

	// defaultMaxAudience is the send's audience ceiling, and since
	// memql#3460 it means something different from what it used to.
	//
	// It WAS a correctness guard at 5000: the worker read the roster whole
	// on every batch, the read was bounded by a `paginate 5000`, and past
	// that bound the send would have diffed a truncated audience and
	// declared itself complete having never seen the tail. Refusing was the
	// only safe answer, and "5000 is the largest mailing list this can send
	// to" was the price.
	//
	// The send now WALKS the roster and reads the ledger per page, so no
	// audience size is unsafe and nothing here is load-bearing for
	// correctness. What remains is a deliberate refusal: a send this large
	// is more often a mis-scoped audience or a bad import than an intent,
	// and a mistake at this scale cannot be recalled. So the ceiling stays,
	// two orders of magnitude higher, and raising it is now a decision about
	// how large a send this deployment means to make -- not a thing you have
	// to do in three places to avoid silent truncation.
	defaultMaxAudience = 250000

	// defaultClaimTTLSeconds leases the cross-replica claim. Sized like
	// the outbound worker's: well above how long one live batch can take
	// (batchSize / rate, plus the send timeout) so a slow-but-alive
	// replica is never stolen from, and far below the guard's 1h
	// retention prune so a replica that died mid-batch does not wedge the
	// campaign until then.
	defaultClaimTTLSeconds = 300

	// defaultThrottleSeconds is how long the worker holds off a job when
	// the provider says "too many requests" WITHOUT a Retry-After. A
	// throttle with no stated duration is still a throttle; guessing one
	// minute is the difference between backing off and hammering.
	defaultThrottleSeconds = 60

	// defaultSendTimeoutSeconds bounds a single message's delivery
	// attempt, so one unresponsive provider connection cannot hold the
	// batch (and therefore the claim) open.
	defaultSendTimeoutSeconds = 30

	// The warming ramp's defaults (memql#3462). The ladder is the shape the
	// runbook's manual procedure already recommended -- start at a few per
	// minute on a new domain and raise it over days -- with the last step at
	// the configured send rate's usual ceiling.
	defaultWarmupSteps = "5,10,25,50,100,200"

	// defaultWarmupMinHoursPerStep is how long a step must run before it can
	// advance. A day, because that is the granularity providers form an
	// opinion at; a step they have not seen through a full daily cycle has
	// not been judged.
	defaultWarmupMinHoursPerStep = 24

	// defaultWarmupMinVolumePerStep is the volume a step must produce before
	// its rates mean anything. THE condition that stops an empty numerator
	// reading as a clean bill of health: four messages with no bounces is a
	// hard bounce rate of 0.0 and no evidence at all.
	defaultWarmupMinVolumePerStep = 200

	// defaultWarmupMinDomainVolume is how much a single domain must have
	// received before its own rate is allowed to hold the ramp. Without it,
	// one bounce at a domain we sent three messages to reads as 33% and pins
	// the ramp permanently.
	defaultWarmupMinDomainVolume = 50

	// defaultWarmupMaxHardBounceRate is the per-domain permanent-bounce
	// ceiling. 2% is the conventional "you are being watched" line; past 5%
	// a sender is usually already in trouble, so holding well under it is
	// the point of ramping at all.
	defaultWarmupMaxHardBounceRate = 0.02

	// defaultWarmupMaxComplaintRate is an order of magnitude tighter than
	// the bounce ceiling, and that asymmetry is the providers' own: a bad
	// address is a hygiene problem, a complaint is a person saying they did
	// not want this. 0.1% is the widely-published threshold.
	defaultWarmupMaxComplaintRate = 0.001
)

// Config is the sending engine's tunable policy, resolved from
// MEMQL_CAMPAIGNS_* env once at construction.
//
// Two of these fields are not tuning at all -- UnsubscribeSecret and
// UnsubscribeBaseURL are PRECONDITIONS. A bulk send with no working
// one-click unsubscribe is not a degraded send, it is a send that must
// not happen, so their absence refuses the campaign at start rather than
// mailing without the header. See RequireUnsubscribe.
type Config struct {
	Enabled            bool
	Poll               time.Duration
	StartupDelay       time.Duration
	BatchSize          int
	SendRatePerMinute  int
	MaxAttempts        int
	MaxAudience        int
	ClaimTTL           time.Duration
	DefaultThrottle    time.Duration
	SendTimeout        time.Duration
	UnsubscribeSecret  string
	UnsubscribeBaseURL string

	// FeedbackSources maps an inbound webhook source name onto the feedback
	// format its payloads are in (memql#3461). Empty means no source on
	// this deployment is trusted to drive suppression, which is the safe
	// default and also the state that ships nothing working -- so the
	// runbook says how to fill it.
	//
	// THIS MAP IS THE AUTHORIZATION for the ingestion path. Deciding that
	// "ses-feedback" is a bounce feed is a deployment-level judgement made
	// by whoever holds the env and the shared HMAC secret -- the same tier
	// as the cluster-wide suppression list it writes to -- rather than a
	// per-call role check on a caller who is, in the end, only asking us to
	// re-read a webhook we already verified.
	FeedbackSources map[string]string

	// SendingIdentity labels the reputation counters and the warming ramp
	// (memql#3462). One deployment normally has exactly one sending mailbox
	// -- the argument the cluster-wide suppression list rests on -- so the
	// default is derived from the configured sender address and an operator
	// sets this only when that derivation is wrong.
	SendingIdentity string

	// NodeID names the replica whose reputation counters this process
	// writes. Not a tuning knob: it is MEMQL_NODE_ID, the same value every
	// pod already carries, and it exists here because counters are written
	// per replica rather than into one contended row.
	NodeID string

	// The warming ramp (memql#3462). OFF by default: an established sending
	// domain does not want its rate re-derived by a control loop that has
	// never seen it.
	WarmupEnabled           bool
	WarmupSteps             []int
	WarmupMinHoursPerStep   time.Duration
	WarmupMinVolumePerStep  int
	WarmupMinDomainVolume   int
	WarmupMaxHardBounceRate float64
	WarmupMaxComplaintRate  float64

	// WarmupStepsError records a malformed MEMQL_CAMPAIGNS_WARMUP_STEPS, so
	// the worker can say so once at boot instead of the ramp being silently
	// absent.
	WarmupStepsError string

	// UnsubscribeSecretPrevious is the secret this node still VERIFIES
	// with but never signs with -- the other half of a rotation
	// (memql#3458). Empty on a deployment that has never rotated, which
	// is also the state the worker warns about at boot once any campaign
	// has sent: from there, changing UnsubscribeSecret alone invalidates
	// every unsubscribe link already in a mailbox.
	UnsubscribeSecretPrevious string
}

// UnsubscribeKeys is the verification ring, in preference order: the
// current secret first (the common case matches on the first try), then
// the previous one if a rotation is in effect.
//
// Deduplicated, because "previous == current" is a rotation somebody
// started and did not finish, and it should cost one comparison rather
// than two. Blank entries are dropped: an unset variable is not a key,
// and an empty HMAC key would verify a token minted by anybody who
// noticed.
func (c Config) UnsubscribeKeys() []string {
	keys := make([]string, 0, 2)
	for _, candidate := range []string{c.UnsubscribeSecret, c.UnsubscribeSecretPrevious} {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		duplicate := false
		for _, held := range keys {
			if held == candidate {
				duplicate = true
			}
		}
		if !duplicate {
			keys = append(keys, candidate)
		}
	}
	return keys
}

// CanRotateUnsubscribeSecret reports whether this node holds a previous
// secret -- that is, whether the NEXT rotation can be survived by links
// already in a mailbox.
func (c Config) CanRotateUnsubscribeSecret() bool {
	return strings.TrimSpace(c.UnsubscribeSecretPrevious) != ""
}

// LoadConfig resolves the policy from the MEMQL_CAMPAIGNS_* env vars, all
// registered in scripts/secrets/manifest.yaml and
// component/genesis/manifest.yaml:
//
//	MEMQL_CAMPAIGNS_ENABLED
//	MEMQL_CAMPAIGNS_POLL_SECONDS
//	MEMQL_CAMPAIGNS_STARTUP_DELAY_SECONDS
//	MEMQL_CAMPAIGNS_BATCH_SIZE
//	MEMQL_CAMPAIGNS_SEND_RATE_PER_MINUTE
//	MEMQL_CAMPAIGNS_MAX_ATTEMPTS
//	MEMQL_CAMPAIGNS_MAX_AUDIENCE
//	MEMQL_CAMPAIGNS_CLAIM_TTL_SECONDS
//	MEMQL_CAMPAIGNS_THROTTLE_SECONDS
//	MEMQL_CAMPAIGNS_SEND_TIMEOUT_SECONDS
//	MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET
//	MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS
//	MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL
//	MEMQL_CAMPAIGNS_FEEDBACK_SOURCES
//	MEMQL_CAMPAIGNS_SENDING_IDENTITY
//	MEMQL_CAMPAIGNS_WARMUP_ENABLED
//	MEMQL_CAMPAIGNS_WARMUP_STEPS
//	MEMQL_CAMPAIGNS_WARMUP_MIN_HOURS_PER_STEP
//	MEMQL_CAMPAIGNS_WARMUP_MIN_VOLUME_PER_STEP
//	MEMQL_CAMPAIGNS_WARMUP_MIN_DOMAIN_VOLUME
//	MEMQL_CAMPAIGNS_WARMUP_MAX_HARD_BOUNCE_RATE
//	MEMQL_CAMPAIGNS_WARMUP_MAX_COMPLAINT_RATE
//
// The names are spelled out here rather than composed in prose because
// the reader builds each one from a prefix plus a suffix, so the full
// name exists nowhere in the source otherwise -- and an operator
// grepping for the variable they were told to set would find nothing.
func LoadConfig() Config {
	reader := env.NewEnvReader("MEMQL_CAMPAIGNS")
	cfg := Config{
		Enabled:           true,
		Poll:              defaultPollSeconds * time.Second,
		StartupDelay:      defaultStartupDelaySeconds * time.Second,
		BatchSize:         defaultBatchSize,
		SendRatePerMinute: defaultSendRatePerMinute,
		MaxAttempts:       defaultMaxAttempts,
		MaxAudience:       defaultMaxAudience,
		ClaimTTL:          defaultClaimTTLSeconds * time.Second,
		DefaultThrottle:   defaultThrottleSeconds * time.Second,
		SendTimeout:       defaultSendTimeoutSeconds * time.Second,

		WarmupMinHoursPerStep:   defaultWarmupMinHoursPerStep * time.Hour,
		WarmupMinVolumePerStep:  defaultWarmupMinVolumePerStep,
		WarmupMinDomainVolume:   defaultWarmupMinDomainVolume,
		WarmupMaxHardBounceRate: defaultWarmupMaxHardBounceRate,
		WarmupMaxComplaintRate:  defaultWarmupMaxComplaintRate,
	}
	cfg.WarmupSteps, _ = parseWarmupSteps(defaultWarmupSteps)
	if b, err := reader.OptionalBool("ENABLED"); err == nil && b != nil {
		cfg.Enabled = *b
	}
	if v, err := reader.OptionalInt("POLL_SECONDS"); err == nil && v != nil && *v > 0 {
		cfg.Poll = time.Duration(*v) * time.Second
	}
	if v, err := reader.OptionalInt("STARTUP_DELAY_SECONDS"); err == nil && v != nil && *v >= 0 {
		cfg.StartupDelay = time.Duration(*v) * time.Second
	}
	if v, err := reader.OptionalInt("BATCH_SIZE"); err == nil && v != nil && *v > 0 {
		cfg.BatchSize = *v
	}
	if v, err := reader.OptionalInt("SEND_RATE_PER_MINUTE"); err == nil && v != nil && *v > 0 {
		cfg.SendRatePerMinute = *v
	}
	if v, err := reader.OptionalInt("MAX_ATTEMPTS"); err == nil && v != nil && *v > 0 {
		cfg.MaxAttempts = *v
	}
	if v, err := reader.OptionalInt("MAX_AUDIENCE"); err == nil && v != nil && *v > 0 {
		cfg.MaxAudience = *v
	}
	if v, err := reader.OptionalInt("CLAIM_TTL_SECONDS"); err == nil && v != nil && *v > 0 {
		cfg.ClaimTTL = time.Duration(*v) * time.Second
	}
	if v, err := reader.OptionalInt("THROTTLE_SECONDS"); err == nil && v != nil && *v > 0 {
		cfg.DefaultThrottle = time.Duration(*v) * time.Second
	}
	if v, err := reader.OptionalInt("SEND_TIMEOUT_SECONDS"); err == nil && v != nil && *v > 0 {
		cfg.SendTimeout = time.Duration(*v) * time.Second
	}
	if s, ok := reader.String("UNSUBSCRIBE_SECRET"); ok {
		cfg.UnsubscribeSecret = s
	}
	if s, ok := reader.String("UNSUBSCRIBE_SECRET_PREVIOUS"); ok {
		cfg.UnsubscribeSecretPrevious = s
	}
	if s, ok := reader.String("UNSUBSCRIBE_BASE_URL"); ok {
		cfg.UnsubscribeBaseURL = s
	}
	if s, ok := reader.String("FEEDBACK_SOURCES"); ok {
		cfg.FeedbackSources = parseFeedbackSources(s)
	}
	if s, ok := reader.String("SENDING_IDENTITY"); ok {
		cfg.SendingIdentity = strings.TrimSpace(s)
	}
	if cfg.SendingIdentity == "" {
		cfg.SendingIdentity = derivedSendingIdentity()
	}
	cfg.NodeID = strings.TrimSpace(os.Getenv("MEMQL_NODE_ID"))
	if b, err := reader.OptionalBool("WARMUP_ENABLED"); err == nil && b != nil {
		cfg.WarmupEnabled = *b
	}
	if s, ok := reader.String("WARMUP_STEPS"); ok {
		steps, err := parseWarmupSteps(s)
		if err != nil {
			// The ramp is left OFF rather than run on a ladder we invented
			// from a malformed one. A misconfigured pacer that silently
			// picks its own rates is worse than one that does not run.
			cfg.WarmupEnabled = false
			cfg.WarmupStepsError = err.Error()
		} else {
			cfg.WarmupSteps = steps
		}
	}
	if v, err := reader.OptionalInt("WARMUP_MIN_HOURS_PER_STEP"); err == nil && v != nil && *v > 0 {
		cfg.WarmupMinHoursPerStep = time.Duration(*v) * time.Hour
	}
	if v, err := reader.OptionalInt("WARMUP_MIN_VOLUME_PER_STEP"); err == nil && v != nil && *v > 0 {
		cfg.WarmupMinVolumePerStep = *v
	}
	if v, err := reader.OptionalInt("WARMUP_MIN_DOMAIN_VOLUME"); err == nil && v != nil && *v > 0 {
		cfg.WarmupMinDomainVolume = *v
	}
	if v, ok := reader.String("WARMUP_MAX_HARD_BOUNCE_RATE"); ok {
		if rate, err := parseRate(v); err == nil {
			cfg.WarmupMaxHardBounceRate = rate
		}
	}
	if v, ok := reader.String("WARMUP_MAX_COMPLAINT_RATE"); ok {
		if rate, err := parseRate(v); err == nil {
			cfg.WarmupMaxComplaintRate = rate
		}
	}
	return cfg
}

// derivedSendingIdentity labels the counters with the mailbox they are about,
// without the operator having to say it twice. The campaign path never picks
// the From address -- the sender does, from its own credential -- so this
// reads the same variables the sender reads.
func derivedSendingIdentity() string {
	for _, name := range []string{"MEMQL_EMAIL_SENDER", "MAIL_SENDER", "SMTP_FROM_ADDR"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return strings.ToLower(v)
		}
	}
	// A label, not a lie: one deployment with one mailbox needs no name for
	// it, and inventing a plausible-looking address would be worse.
	return "default"
}

// parseRate reads a threshold as a fraction (0.02) or a percentage ("2%").
// Both spellings appear in deliverability documentation and an operator who
// types the one this did not expect should not silently get a threshold two
// orders of magnitude off.
func parseRate(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	percent := strings.HasSuffix(value, "%")
	value = strings.TrimSuffix(value, "%")
	var f float64
	if _, err := fmt.Sscanf(value, "%g", &f); err != nil {
		return 0, fmt.Errorf("%q is not a rate", raw)
	}
	if percent {
		f /= 100
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("rate %q is outside 0..1", raw)
	}
	return f, nil
}

// parseFeedbackSources reads `source=format,source=format`.
//
// An entry naming an unsupported format is DROPPED rather than defaulted,
// and FeedbackFormatFor's caller reports the source as unconfigured. A
// typo'd format silently resolving to "the standard one" would mean parsing
// SES JSON as a DSN, which produces no reports and looks exactly like a
// deployment with no bounces -- the failure this whole task exists to
// remove.
func parseFeedbackSources(raw string) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		name, format, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		format = strings.ToLower(strings.TrimSpace(format))
		if name == "" {
			continue
		}
		for _, supported := range SupportedFeedbackFormats() {
			if format == supported {
				out[name] = format
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FeedbackFormatFor reports the format configured for an inbound source, or
// "" when the source is not a trusted feedback feed on this deployment.
func (c Config) FeedbackFormatFor(source string) string {
	return c.FeedbackSources[strings.TrimSpace(source)]
}

// RequireUnsubscribe reports the reason a send cannot legally start, or
// "" when one-click unsubscribe is fully configured.
//
// FAIL CLOSED IS THE POINT. Every large mailbox provider now treats a
// missing List-Unsubscribe as a bulk-sender defect, and CAN-SPAM / GDPR /
// CASL each require a working opt-out -- so "send anyway, without the
// header" is not a lesser version of the feature, it is the version that
// gets the sending domain blocked and the operator fined. The check is
// here, in config, rather than at the send site, because it must be
// answerable BEFORE a campaign is started: refusing 4000 messages in is
// worse than refusing at the button.
func (c Config) RequireUnsubscribe() string {
	if c.UnsubscribeSecret == "" {
		return "MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET is not set, so no one-click unsubscribe link can be signed. " +
			"A bulk send without a working RFC 8058 unsubscribe is refused rather than sent."
	}
	if c.UnsubscribeBaseURL == "" {
		return "MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL is not set, so no one-click unsubscribe URL can be built. " +
			"A bulk send without a working RFC 8058 unsubscribe is refused rather than sent."
	}
	if reason := validateUnsubscribeBaseURL(c.UnsubscribeBaseURL); reason != "" {
		return reason
	}
	return ""
}

// validateUnsubscribeBaseURL reports why the configured origin cannot carry a
// conforming one-click unsubscribe link, or "" if it can.
//
// RFC 8058 §3.1 is a MUST, not a SHOULD: "The List-Unsubscribe header field
// MUST contain one HTTPS URI." A value that is merely non-empty satisfies the
// presence check above and still produces a header the mailbox provider
// silently declines to act on -- the campaign sends, every check is green, and
// the first evidence is a complaint rate nobody can attribute (memql#3481).
//
// A VALID HTTPS ORIGIN IS NECESSARY, NOT SUFFICIENT. §3.1 also requires the
// header PAIR (List-Unsubscribe alongside List-Unsubscribe-Post:
// List-Unsubscribe=One-Click); renderMessage already emits both, so this
// function owns only the URI half.
//
// The rules, and why each one:
//
//   - HTTPS required, EXCEPT on a loopback host. `make up` runs the bff behind
//     a local origin with no public certificate, and refusing that would make
//     the local cluster the one environment where campaigns cannot be
//     exercised. A loopback URL cannot reach a real mailbox provider, so
//     permitting it costs nothing.
//   - A non-empty host. This is what catches the bare `cockpit.example.com`
//     case: url.Parse accepts it, puts the whole value in Path, and leaves
//     Scheme and Host empty -- so without this check it concatenates into
//     `cockpit.example.com/unsubscribe`, a relative URI in a header that
//     requires an absolute one.
//   - No query or fragment. unsubscribeURL appends `?token=`, so a base that
//     already carries one produces a URL with two.
//
// A trailing PATH is allowed and preserved. UnsubscribePaths() composes
// through pathsWithBase(), so a deployment served under SERVER_PUBLIC_PATH
// legitimately has one; `https://host/app` yields
// `https://host/app/unsubscribe`.
func validateUnsubscribeBaseURL(raw string) string {
	const varName = "MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL"

	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Sprintf("%s is not a valid URL (%v). "+
			"A bulk send without a working RFC 8058 unsubscribe is refused rather than sent.", varName, err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Sprintf("%s must be an absolute origin such as https://cockpit.example.com, but is %q. "+
			"A value with no scheme is not a URL the recipient's mail client can POST to -- it would be "+
			"concatenated into a relative path. A bulk send without a working RFC 8058 unsubscribe is "+
			"refused rather than sent.", varName, trimmed)
	}

	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return fmt.Sprintf("%s must use https, but is %q. RFC 8058 section 3.1 requires the "+
			"List-Unsubscribe header to carry an HTTPS URI, and mailbox providers silently ignore "+
			"one-click on anything else -- so the campaign would send with an opt-out that never "+
			"works. (http is permitted only for a loopback host, so the local cluster still runs.) "+
			"A bulk send without a working RFC 8058 unsubscribe is refused rather than sent.", varName, trimmed)
	}

	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Sprintf("%s must not carry a query string or fragment, but is %q. "+
			"The unsubscribe token is appended as ?token=..., so a base that already has one "+
			"produces a URL with two. A trailing path is fine and is preserved.", varName, trimmed)
	}

	return ""
}

// isLoopbackHost reports whether the host names this machine -- the one case
// where plain http is permitted, because the local cluster has no public
// certificate and a loopback URL cannot reach a mailbox provider anyway.
func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
