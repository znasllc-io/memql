package safety

import "strings"

// secretKeyFragments is the case-insensitive substring set used to
// identify secret-bearing map keys (TOKEN / SECRET / PASSWORD /
// API_KEY / etc.). Mirrors the worker-side redaction in
// integrations/agent/worker/dispatch.go so a value scrubbed there
// is also scrubbed here, and vice versa. Kept in safety so the
// package stays import-free.
var secretKeyFragments = []string{
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"PASSWD",
	"API_KEY",
	"APIKEY",
	"PRIVATE_KEY",
	"PRIVATEKEY",
}

// IsSecretKey returns true if `k` (case-insensitive) names a field
// that almost certainly carries a credential. Conservative match --
// the cost of a false positive is a redacted log line; the cost of
// a false negative is a credential in audit storage.
func IsSecretKey(k string) bool {
	upper := strings.ToUpper(k)
	if upper == "AUTHORIZATION" {
		return true
	}
	for _, frag := range secretKeyFragments {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}

// RedactArgs returns a copy of `in` with every secret-shaped key
// (per IsSecretKey) replaced by "[REDACTED]". Nested maps are
// scrubbed recursively; non-map values pass through unchanged. The
// returned map is always independent of the caller's -- safe to log
// or persist without aliasing. Returns nil for an empty input.
func RedactArgs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if IsSecretKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			out[k] = RedactArgs(nested)
			continue
		}
		out[k] = v
	}
	return out
}

// RedactedPayload returns a copy of `p` with secret-bearing fields
// scrubbed: a non-empty Body is replaced wholesale (we don't try to
// parse it -- a credential can hide anywhere in a JSON/x-www-form
// body), and Args is recursively redacted. Command / Paths / URL /
// Method / ToolName are pass-through -- they're inspected by the
// classifier and shouldn't carry raw credentials in practice; if
// they do (e.g. a URL with userinfo), the rule engine flags it as
// credential_access.
func RedactedPayload(p Payload) Payload {
	out := p
	if out.Body != "" {
		out.Body = "[REDACTED]"
	}
	out.Args = RedactArgs(p.Args)
	return out
}
