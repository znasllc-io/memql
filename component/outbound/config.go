// Package outbound implements the engine-drained outbound-delivery
// worker (memql#2521): products stage v1:platform:outboundRequest rows
// from pure DSL; this worker drains pending/retrying rows, performs the
// delivery through deploy-configured transports (email / webhook), and
// stamps status transitions back onto the rows. Design and delivery
// contract: docs/internal/design/outbound-delivery-adr.md.
package outbound

import (
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/env"
)

const (
	// defaultPollSeconds is the safety-net drain interval. The
	// graph.node.created fast-path drains new rows within a second on
	// the replica that executed the staging mutation; the poll exists
	// for cross-replica pickup, retry due-times, and dropped events.
	defaultPollSeconds = 15

	// defaultStartupDelaySeconds defers the first drain so the engine +
	// DSL finish loading before the worker issues its first query
	// (watchdog precedent: the engine rejects queries mid-bootstrap).
	defaultStartupDelaySeconds = 20

	// defaultMaxAttempts bounds delivery attempts before a row is
	// stamped failed.
	defaultMaxAttempts = 5

	// defaultMaxPayloadBytes caps the payload accepted at drain time.
	// Rows above the cap fail permanently (ADR 4.3).
	defaultMaxPayloadBytes = 262144

	// defaultHTTPTimeoutSeconds bounds a single webhook POST.
	defaultHTTPTimeoutSeconds = 10

	// defaultClaimTTLSeconds is the attempt-scoped lease on the
	// cross-replica claim (memql#2548). A claim orphaned by a replica that
	// died between winning it and stamping a terminal status becomes
	// re-winnable after this many seconds, so a peer recovers the row
	// instead of it wedging until the guard's 1h retention prune. It must
	// sit well above the longest a LIVE claimant holds a claim -- one
	// delivery attempt, bounded by the HTTP timeout -- so a slow-but-alive
	// node is not stolen from; 5 minutes gives ~30x margin over the default
	// 10s webhook timeout while cutting the wedge from 1h to minutes.
	defaultClaimTTLSeconds = 300
)

// Config is the worker's tunable policy, resolved from env once at
// construction (MEMQL_OUTBOUND_*). Deny-by-default: a medium without a
// non-empty allowlist is disabled, and rows targeting it fail fast with
// an explicit error rather than waiting silently (ADR 4.3).
type Config struct {
	Enabled          bool
	Poll             time.Duration
	StartupDelay     time.Duration
	MaxAttempts      int
	EmailAllowlist   []string // recipient domain suffixes, lowercased
	WebhookAllowlist []string // URL prefixes; https required unless the prefix itself is http://
	MaxPayloadBytes  int
	HTTPTimeout      time.Duration
	ClaimTTL         time.Duration // attempt-scoped lease on the cross-replica claim (memql#2548)
}

// LoadConfig resolves the worker policy from the MEMQL_OUTBOUND_* env
// vars (all registered in scripts/secrets/manifest.yaml):
// MEMQL_OUTBOUND_ENABLED, MEMQL_OUTBOUND_POLL_SECONDS,
// MEMQL_OUTBOUND_STARTUP_DELAY_SECONDS, MEMQL_OUTBOUND_MAX_ATTEMPTS,
// MEMQL_OUTBOUND_MAX_PAYLOAD_BYTES, MEMQL_OUTBOUND_HTTP_TIMEOUT_SECONDS,
// MEMQL_OUTBOUND_CLAIM_TTL_SECONDS, MEMQL_OUTBOUND_EMAIL_ALLOWLIST,
// MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST.
func LoadConfig() Config {
	reader := env.NewEnvReader("MEMQL_OUTBOUND")
	cfg := Config{
		Enabled:         true,
		Poll:            defaultPollSeconds * time.Second,
		StartupDelay:    defaultStartupDelaySeconds * time.Second,
		MaxAttempts:     defaultMaxAttempts,
		MaxPayloadBytes: defaultMaxPayloadBytes,
		HTTPTimeout:     defaultHTTPTimeoutSeconds * time.Second,
		ClaimTTL:        defaultClaimTTLSeconds * time.Second,
	}
	if b, err := reader.OptionalBool("ENABLED"); err == nil && b != nil {
		cfg.Enabled = *b
	}
	if ptr, err := reader.OptionalInt("POLL_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.Poll = time.Duration(*ptr) * time.Second
	}
	if ptr, err := reader.OptionalInt("STARTUP_DELAY_SECONDS"); err == nil && ptr != nil && *ptr >= 0 {
		cfg.StartupDelay = time.Duration(*ptr) * time.Second
	}
	if ptr, err := reader.OptionalInt("MAX_ATTEMPTS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.MaxAttempts = *ptr
	}
	if ptr, err := reader.OptionalInt("MAX_PAYLOAD_BYTES"); err == nil && ptr != nil && *ptr > 0 {
		cfg.MaxPayloadBytes = *ptr
	}
	if ptr, err := reader.OptionalInt("HTTP_TIMEOUT_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.HTTPTimeout = time.Duration(*ptr) * time.Second
	}
	if ptr, err := reader.OptionalInt("CLAIM_TTL_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.ClaimTTL = time.Duration(*ptr) * time.Second
	}
	if s, ok := reader.String("EMAIL_ALLOWLIST"); ok {
		cfg.EmailAllowlist = splitAllowlist(s, true)
	}
	if s, ok := reader.String("WEBHOOK_ALLOWLIST"); ok {
		cfg.WebhookAllowlist = normalizeWebhookPrefixes(splitAllowlist(s, false))
	}
	return cfg
}

// normalizeWebhookPrefixes guarantees every allowlist entry contains a
// path separator after the authority ("https://host" -> "https://host/").
// Without it, prefix matching alone admits userinfo tricks: the target
// "https://host@evil.example/x" starts with the prefix "https://host"
// but its real host is evil.example. With the trailing slash the
// authority is closed and a matched target can only extend the path.
func normalizeWebhookPrefixes(prefixes []string) []string {
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if idx := strings.Index(p, "://"); idx >= 0 && !strings.Contains(p[idx+3:], "/") {
			p += "/"
		}
		out = append(out, p)
	}
	return out
}

// splitAllowlist parses a comma-separated allowlist env value. Entries
// are trimmed; empties dropped; domain lists lowercased (targets are
// compared case-insensitively, URL prefixes are not).
func splitAllowlist(raw string, lower bool) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lower {
			part = strings.ToLower(part)
		}
		out = append(out, part)
	}
	return out
}

// emailAllowed reports whether a recipient address matches the domain
// suffix allowlist. The match is on the domain part only, case
// insensitive; "example.com" also admits subdomains ("a.example.com").
// An empty allowlist admits nothing (medium disabled).
func emailAllowed(target string, domains []string) bool {
	at := strings.LastIndex(target, "@")
	if at <= 0 || at == len(target)-1 {
		return false
	}
	domain := strings.ToLower(target[at+1:])
	for _, d := range domains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// matchWebhookPrefix returns the allowlist prefix the target matches
// (and the remainder after it), applying the scheme policy: HTTPS is
// required unless the matching prefix itself is an explicit http://
// entry (cluster-internal hosts the operator opted in, ADR 4.3). An
// empty allowlist matches nothing (medium disabled). Prefixes are
// normalized to close the authority (normalizeWebhookPrefixes), so the
// remainder can only extend the path/query -- the transport rebuilds
// the request URL as prefix + remainder, keeping the host
// config-owned.
func matchWebhookPrefix(target string, prefixes []string) (prefix, remainder string, ok bool) {
	for _, p := range prefixes {
		if !strings.HasPrefix(target, p) {
			continue
		}
		if strings.HasPrefix(strings.ToLower(target), "https://") ||
			strings.HasPrefix(strings.ToLower(p), "http://") {
			return p, strings.TrimPrefix(target, p), true
		}
	}
	return "", "", false
}

// webhookAllowed reports whether a target URL matches the prefix
// allowlist (see matchWebhookPrefix).
func webhookAllowed(target string, prefixes []string) bool {
	_, _, ok := matchWebhookPrefix(target, prefixes)
	return ok
}
