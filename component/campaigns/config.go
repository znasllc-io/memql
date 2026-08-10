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
	}
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
	return cfg
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
	return ""
}
