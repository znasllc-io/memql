// Package inbound implements the engine's inbound-delivery receiver
// (memql#2957): a signature-verifying webhook endpoint that stages
// v1:platform:inboundRequest rows which product DSL then drains with an
// ordinary automation. It is the counterpart to component/outbound --
// outbound is the outbox a product stages and the engine drains, inbound is
// the inbox the engine stages and a product drains. Design and delivery
// contract: docs/internal/design/inbound-delivery-adr.md.
package inbound

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/env"
)

const (
	// defaultMaxBodyBytes caps the request body the receiver will read.
	// Anything larger is refused with 413 and no row is staged -- the cap
	// exists so an unauthenticated caller cannot spend the node's memory
	// before the signature check has had a chance to reject it.
	defaultMaxBodyBytes = 262144

	// defaultToleranceSeconds is the replay window for sources that sign a
	// timestamp alongside the body. A captured request older (or newer, for
	// clock skew) than this is refused even when its signature is valid.
	defaultToleranceSeconds = 300
)

// Signature schemes. The engine deliberately carries no vendor table: every
// third party spells the same HMAC-SHA256 differently, so a source names the
// ENCODING it uses and, if it needs one, the literal prefix to strip. That is
// enough for Shopify (base64, bare), GitHub (hex, "sha256=" prefix) and the
// timestamped variants, without a line of vendor code here.
const (
	SchemeHMACSHA256Hex    = "hmac-sha256-hex"
	SchemeHMACSHA256Base64 = "hmac-sha256-base64"
	// SchemeNone accepts without verifying. It exists for a sender that is
	// already behind another trust boundary (a mesh-internal producer, a
	// gateway that verified upstream) and it must be spelled out per source:
	// an unset scheme is an error, never a silent downgrade to this.
	SchemeNone = "none"
)

// sourceNamePattern bounds a source name to what can appear in a single URL
// path segment and in an env var name. It also guarantees the name is free of
// the NUL byte requestIdFor uses as its separator, which is what makes the
// (source, identityKey) composition injective -- see requestIdFor.
var sourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// SourceConfig is one allowlisted sender's verification policy, resolved from
// env at construction. The secret lives here and NOWHERE else -- never on the
// staged row, never in a log line, never in an error returned to the caller.
type SourceConfig struct {
	Name string
	// Secret is the shared HMAC key. Required unless Scheme is SchemeNone.
	Secret string
	// Scheme is one of the Scheme* constants.
	Scheme string
	// SignatureHeader carries the sender's signature (e.g. "X-Hub-Signature-256").
	SignatureHeader string
	// SignaturePrefix is stripped from the header value before decoding
	// (e.g. "sha256="). Empty when the sender sends a bare digest.
	SignaturePrefix string
	// TimestampHeader, when set, makes the signed payload "<timestamp>.<body>"
	// and brings the replay window into force. Empty means the sender signs
	// the body alone and replay protection is the product's problem -- which
	// is why dedupeKey exists.
	//
	// A sender using ElementSeparator carries its timestamp INSIDE
	// SignatureHeader; set TimestampHeader to the same header name and name
	// the element in TimestampElement.
	TimestampHeader string
	// ElementSeparator, when set, says the signature header is a list of
	// `key=value` elements rather than a single value -- e.g.
	// `t=1614556800,v1=5257a8...`, where "," is the separator. SignatureElement
	// then names the key carrying the digest and TimestampElement the key
	// carrying the timestamp.
	//
	// WHY THIS IS AN ENCODING AND NOT A VENDOR (memql#3854). The scheme
	// constants above are deliberately encodings, so that a new sender is a
	// config change rather than a release, and the comment on them says so at
	// length. A composite header is the same KIND of fact: it describes how the
	// bytes are laid out, not who sent them. Naming the separator and the two
	// element keys keeps that property -- there is still no vendor table, and
	// the next sender that packs its signature into one header is configured
	// rather than coded.
	//
	// It exists because the alternative was worse. Stripe -- the sender this
	// epic needs -- sends `Stripe-Signature: t=<unix>,v1=<hex>`: one header,
	// both values, and `v1=` is not a leading PREFIX because `t=<unix>,` comes
	// first. So SignaturePrefix cannot reach it, TimestampHeader has no
	// separate header to read, and a source configured as carefully as the
	// documentation allows still refuses every delivery with a flat 401. The
	// only other route was a new HTTP endpoint, which the whole inbound design
	// exists to avoid.
	ElementSeparator string
	// SignatureElement names the element carrying the digest (e.g. "v1").
	// Required when ElementSeparator is set.
	SignatureElement string
	// TimestampElement names the element carrying the timestamp (e.g. "t").
	// Optional: a composite header that carries no timestamp is a sender that
	// signs the body alone.
	TimestampElement string
	// DedupeHeader names the sender's own idempotency key. Empty falls back to
	// the SHA-256 of the body, which collapses byte-identical redeliveries.
	DedupeHeader string
}

// Config is the receiver's policy. Deny-by-default in the strongest sense:
// Sources is empty unless an operator lists names in
// MEMQL_INBOUND_SOURCE_ALLOWLIST, and a listed source that does not resolve to
// a usable verification policy is DROPPED rather than admitted unverified.
type Config struct {
	Enabled      bool
	MaxBodyBytes int
	Tolerance    time.Duration
	Sources      map[string]SourceConfig
}

// LoadConfig resolves the receiver policy from the MEMQL_INBOUND_* env vars.
//
// Global:
//
//	MEMQL_INBOUND_ENABLED                      default true
//	MEMQL_INBOUND_SOURCE_ALLOWLIST             comma-separated names; EMPTY = the
//	                                           receiver admits nothing
//	MEMQL_INBOUND_MAX_BODY_BYTES               default 262144
//	MEMQL_INBOUND_TIMESTAMP_TOLERANCE_SECONDS  default 300
//
// Per source <NAME> (uppercased, '-' -> '_'):
//
//	MEMQL_INBOUND_SOURCE_<NAME>_SECRET
//	MEMQL_INBOUND_SOURCE_<NAME>_SIGNATURE_SCHEME   hmac-sha256-hex | hmac-sha256-base64 | none
//	MEMQL_INBOUND_SOURCE_<NAME>_SIGNATURE_HEADER
//	MEMQL_INBOUND_SOURCE_<NAME>_SIGNATURE_PREFIX
//	MEMQL_INBOUND_SOURCE_<NAME>_TIMESTAMP_HEADER
//	MEMQL_INBOUND_SOURCE_<NAME>_DEDUPE_HEADER
//	MEMQL_INBOUND_SOURCE_<NAME>_ELEMENT_SEPARATOR   e.g. "," for a k=v list header
//	MEMQL_INBOUND_SOURCE_<NAME>_SIGNATURE_ELEMENT   e.g. "v1"
//	MEMQL_INBOUND_SOURCE_<NAME>_TIMESTAMP_ELEMENT   e.g. "t"
//
// Misconfiguration is reported through the logger and costs the source its
// slot; it never costs the node its boot, and it never admits the source
// unverified. A receiver serving one of five sources is a visible, partial
// outage; a receiver serving five sources with one of them unsigned is a
// silent hole.
func LoadConfig(logger *slog.Logger) Config {
	reader := env.NewEnvReader("MEMQL_INBOUND")
	cfg := Config{
		Enabled:      true,
		MaxBodyBytes: defaultMaxBodyBytes,
		Tolerance:    defaultToleranceSeconds * time.Second,
		Sources:      map[string]SourceConfig{},
	}
	if b, err := reader.OptionalBool("ENABLED"); err == nil && b != nil {
		cfg.Enabled = *b
	}
	if ptr, err := reader.OptionalInt("MAX_BODY_BYTES"); err == nil && ptr != nil && *ptr > 0 {
		cfg.MaxBodyBytes = *ptr
	}
	if ptr, err := reader.OptionalInt("TIMESTAMP_TOLERANCE_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.Tolerance = time.Duration(*ptr) * time.Second
	}

	raw, _ := reader.String("SOURCE_ALLOWLIST")
	for _, name := range splitAllowlist(raw) {
		src, err := loadSource(name)
		if err != nil {
			if logger != nil {
				logger.Error("inbound receiver: source dropped, it will be refused with 404",
					"source", name, "error", err)
			}
			continue
		}
		if src.Scheme == SchemeNone && logger != nil {
			logger.Warn("inbound receiver: source accepts UNVERIFIED requests",
				"source", name,
				"why", "MEMQL_INBOUND_SOURCE_"+envSuffix(name)+"_SIGNATURE_SCHEME=none")
		}
		cfg.Sources[src.Name] = src
	}
	return cfg
}

// loadSource resolves one source's policy, failing closed on anything it
// cannot make sense of.
func loadSource(name string) (SourceConfig, error) {
	if !sourceNamePattern.MatchString(name) {
		return SourceConfig{}, fmt.Errorf("source name %q is not a valid path segment "+
			"(want %s)", name, sourceNamePattern)
	}
	prefix := "MEMQL_INBOUND_SOURCE_" + envSuffix(name) + "_"
	src := SourceConfig{
		Name:             name,
		Secret:           os.Getenv(prefix + "SECRET"),
		Scheme:           strings.TrimSpace(os.Getenv(prefix + "SIGNATURE_SCHEME")),
		SignatureHeader:  strings.TrimSpace(os.Getenv(prefix + "SIGNATURE_HEADER")),
		SignaturePrefix:  strings.TrimSpace(os.Getenv(prefix + "SIGNATURE_PREFIX")),
		TimestampHeader:  strings.TrimSpace(os.Getenv(prefix + "TIMESTAMP_HEADER")),
		DedupeHeader:     strings.TrimSpace(os.Getenv(prefix + "DEDUPE_HEADER")),
		ElementSeparator: strings.TrimSpace(os.Getenv(prefix + "ELEMENT_SEPARATOR")),
		SignatureElement: strings.TrimSpace(os.Getenv(prefix + "SIGNATURE_ELEMENT")),
		TimestampElement: strings.TrimSpace(os.Getenv(prefix + "TIMESTAMP_ELEMENT")),
	}

	switch src.Scheme {
	case SchemeNone:
		return src, nil
	case SchemeHMACSHA256Hex, SchemeHMACSHA256Base64:
	case "":
		return SourceConfig{}, fmt.Errorf("%sSIGNATURE_SCHEME is unset -- set it to %q, %q or, "+
			"to accept unverified, an explicit %q", prefix,
			SchemeHMACSHA256Hex, SchemeHMACSHA256Base64, SchemeNone)
	default:
		return SourceConfig{}, fmt.Errorf("%sSIGNATURE_SCHEME=%q is not a known scheme (want %q, %q or %q)",
			prefix, src.Scheme, SchemeHMACSHA256Hex, SchemeHMACSHA256Base64, SchemeNone)
	}
	if src.Secret == "" {
		return SourceConfig{}, fmt.Errorf("%sSECRET is empty, so the signature could not be checked", prefix)
	}
	if src.SignatureHeader == "" {
		return SourceConfig{}, fmt.Errorf("%sSIGNATURE_HEADER is unset, so there is nothing to check", prefix)
	}

	// The composite-header trio is all-or-nothing, and each half is refused
	// separately because each half fails DIFFERENTLY and neither failure names
	// itself. A separator without an element key leaves nothing to select, so
	// every delivery 401s on a digest that is really a whole header. An element
	// key without a separator is silently ignored, so the source verifies
	// against the raw header and 401s too -- the same flat status code, from a
	// configuration that looks more complete than the one that would work.
	if src.SignatureElement != "" && src.ElementSeparator == "" {
		return SourceConfig{}, fmt.Errorf("%sSIGNATURE_ELEMENT is set but %sELEMENT_SEPARATOR is not, "+
			"so the header would be checked whole and every delivery would be refused", prefix, prefix)
	}
	if src.TimestampElement != "" && src.ElementSeparator == "" {
		return SourceConfig{}, fmt.Errorf("%sTIMESTAMP_ELEMENT is set but %sELEMENT_SEPARATOR is not, "+
			"so no timestamp would be read and the replay window would be off", prefix, prefix)
	}
	if src.ElementSeparator != "" {
		if src.SignatureElement == "" {
			return SourceConfig{}, fmt.Errorf("%sELEMENT_SEPARATOR is set but %sSIGNATURE_ELEMENT is not, "+
				"so there is no way to say which element carries the digest", prefix, prefix)
		}
		// A composite header carries its timestamp inside itself, so pointing
		// TimestampHeader at a DIFFERENT header is a configuration that cannot
		// be satisfied -- the value it would read was never signed.
		if src.TimestampElement != "" && src.TimestampHeader != "" && src.TimestampHeader != src.SignatureHeader {
			return SourceConfig{}, fmt.Errorf("%sTIMESTAMP_HEADER=%q differs from %sSIGNATURE_HEADER=%q "+
				"while a TIMESTAMP_ELEMENT is set: a composite header carries its timestamp inside itself, "+
				"so the other header's value was never signed", prefix, src.TimestampHeader, prefix, src.SignatureHeader)
		}
	}
	return src, nil
}

// envSuffix maps a source name onto the env-var spelling of it.
func envSuffix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// splitAllowlist parses the comma-separated allowlist. Entries are trimmed and
// lowercased (the URL segment is matched case-sensitively against the
// lowercased name); empties are dropped.
func splitAllowlist(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}
