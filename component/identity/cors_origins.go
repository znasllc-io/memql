package identity

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// cors_origins.go -- the origin grammar behind the admin-granted half of
// identity's CORS allowlist (memql#3716).
//
// It lives in this package, not in component/identity/http, because BOTH ends
// need the same answer and they sit in different packages: adminops validates
// on the way IN (where a human is holding an error message) and the store drops
// what it cannot recognise on the way OUT (where nobody is). Two copies of this
// grammar would eventually disagree, and the direction they would disagree in
// is the fail-open one -- a write accepting something the read then matches
// loosely.

// MaxCORSOriginsPerClient caps how many origins one client's allowance may
// carry.
//
// Not a storage concern: the cap exists because the granted set is unioned into
// the allowlist the middleware scans on every preflight, and because an
// allowance nobody can read is an allowance nobody audits. A customer running
// one site needs one entry; the ceiling is generous for a customer with a few
// hostnames and still small enough that an operator can read the row.
const MaxCORSOriginsPerClient = 20

// ValidateCORSOrigin checks one entry and returns its canonical form.
//
// An origin is a scheme, a host, and an optional port -- RFC 6454 -- and
// nothing else. Everything below is refused rather than normalised away,
// because each refused shape is a case where the operator and the browser would
// disagree about what was granted:
//
//   - a path, query or fragment: the browser sends the origin only, so
//     "https://x.example/app" would be stored, never matched, and read as a
//     working grant that silently does nothing;
//   - a trailing slash: the same failure with less to see -- it is a path of
//     "/", and no browser ever sends one in an Origin header;
//   - userinfo: credentials in an allowlist entry, matched against a header
//     that cannot carry them;
//   - "*": the wildcard is refused everywhere on this path. cors() sets
//     Access-Control-Allow-Credentials on every match, so one wildcard would
//     grant credentialed cross-origin read access to every origin on the
//     internet and make the owner/admin gate decorative.
//
// The returned form is scheme-and-host lowercased with the port preserved,
// which is the ASCII-serialised origin a browser sends. Matching downstream is
// case-insensitive anyway; canonicalising here is so the STORED row reads the
// way the header will arrive.
func ValidateCORSOrigin(raw string) (string, error) {
	origin := strings.TrimSpace(raw)
	if origin == "" {
		return "", fmt.Errorf("an origin cannot be empty")
	}
	if origin == "*" {
		return "", fmt.Errorf(`%q is not a grantable origin: this allowlist gates CREDENTIALED `+
			`cross-origin requests, so a wildcard would grant every origin on the internet `+
			`cookie-bearing read access to the auth endpoints`, origin)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid origin: %w", origin, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
	case "":
		return "", fmt.Errorf(`%q is not a valid origin: it needs a scheme, e.g. "https://%s"`, origin, origin)
	default:
		return "", fmt.Errorf("%q is not a valid origin: scheme must be http or https, got %q", origin, scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q is not a valid origin: it names no host", origin)
	}
	if u.User != nil {
		return "", fmt.Errorf("%q is not a valid origin: it carries userinfo, which an Origin header cannot", origin)
	}
	if u.Path != "" {
		return "", fmt.Errorf(`%q is not a valid origin: an origin is scheme + host + optional port `+
			`with no path (a browser sends "%s://%s"), so this entry would never match`,
			origin, scheme, u.Host)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%q is not a valid origin: it carries a query string, which an Origin header never does", origin)
	}
	if u.Fragment != "" || strings.Contains(origin, "#") {
		return "", fmt.Errorf("%q is not a valid origin: it carries a fragment, which an Origin header never does", origin)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("%q is not a valid origin: it is not an absolute scheme://host reference", origin)
	}
	// Hostname() strips the port and any IPv6 brackets. The wildcard check comes
	// first so it gets its own message: a subdomain wildcard is not a smaller
	// version of "*" on this path, it is still an unbounded set of origins whose
	// members the person granting it does not all control.
	host := u.Hostname()
	if strings.Contains(host, "*") {
		return "", fmt.Errorf(`%q is not a valid origin: a wildcard host names an unbounded set of `+
			`origins, and each one would receive credentialed access -- name each origin`, origin)
	}
	// The host must be a DNS name, an IPv4 literal, or a bracketed IPv6 literal
	// -- checked explicitly rather than left to net/url, which is markedly more
	// permissive. It accepts a DOUBLE QUOTE inside a host, for one, and that
	// value would then be stored in a row and compared against an Origin header
	// that can never carry it: an entry that reads as granted and matches
	// nothing. TestValidateCORSOriginCannotSmuggleStringLiteralSyntax is what
	// measures this rather than assuming it.
	if net.ParseIP(host) == nil && !isOriginDNSName(host) {
		return "", fmt.Errorf(`%q is not a valid origin: %q is not a host a browser can send. `+
			`It must be a DNS name, an IPv4 address, or a bracketed IPv6 address -- and an `+
			`internationalised domain must be given in the punycode ("xn--") form, which is `+
			`what the browser puts in the Origin header`, origin, host)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

// isOriginDNSName reports whether host is a plausible DNS hostname.
//
// Letters, digits, hyphens, dots -- plus UNDERSCORE, which RFC 1123 does not
// permit and real deployments use anyway (container and internal service names).
// Being stricter than reality here would refuse an origin somebody legitimately
// serves from, which is a worse failure than admitting an underscore: nothing in
// the set can alter a string literal or a header comparison, which is the
// property that actually matters.
//
// No trailing dot: a browser does not send one, so an entry carrying it would
// never match.
func isOriginDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z',
				c >= 'A' && c <= 'Z',
				c >= '0' && c <= '9',
				c == '-', c == '_':
			default:
				return false
			}
		}
	}
	return true
}

// ValidateCORSOrigins validates a whole allowance and returns the canonical
// list. An empty or nil input is valid and means "no allowance" -- that is how
// a grant is revoked.
//
// Every entry is reported by VALUE in the error, because the caller is a person
// who typed a list and needs to know which line is wrong. Duplicates are
// collapsed rather than refused: two spellings of one origin is a typo with no
// consequence, and refusing it would be a worse experience than fixing it.
func ValidateCORSOrigins(in []string) ([]string, error) {
	if len(in) > MaxCORSOriginsPerClient {
		return nil, fmt.Errorf("too many origins: %d, the maximum is %d", len(in), MaxCORSOriginsPerClient)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		origin, err := ValidateCORSOrigin(raw)
		if err != nil {
			return nil, err
		}
		if seen[origin] {
			continue
		}
		seen[origin] = true
		out = append(out, origin)
	}
	return out, nil
}

// MarshalCORSOrigins renders a validated allowance for the corsOriginsJSON
// field. Always a JSON array, "[]" for a revoke -- never an empty string, so
// the concept field holds one representation of "no allowance" rather than two.
func MarshalCORSOrigins(origins []string) (string, error) {
	if origins == nil {
		origins = []string{}
	}
	encoded, err := json.Marshal(origins)
	if err != nil {
		return "", fmt.Errorf("identity: marshal cors origins: %w", err)
	}
	return string(encoded), nil
}

// ParseCORSOriginsJSON reads a stored corsOriginsJSON value and returns the
// origins that are still recognisable, along with the entries that were
// dropped.
//
// It is LENIENT where ValidateCORSOrigins is strict, and the asymmetry is
// deliberate. The write path refuses a malformed entry so a human can fix it;
// the read path runs on an unauthenticated preflight with nobody to tell. If
// one bad entry could fail the whole read, one bad row would take every OTHER
// customer's site down with it -- so a bad entry is dropped and the good ones
// stand. Dropping is the fail-CLOSED direction for the entry itself: an origin
// we cannot parse is an origin we do not allow.
//
// A malformed value can only get here from a direct server-side write, since
// nothing client-reachable can set the field. The returned dropped list exists
// so the caller can say so out loud rather than swallowing it.
func ParseCORSOriginsJSON(raw string) (origins []string, dropped []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		// The whole value is unreadable, so there is nothing to salvage. Report
		// it as one dropped entry rather than returning an error: the caller's
		// only useful response either way is to allow nothing from this row and
		// log it.
		return nil, []string{raw}
	}
	for _, entry := range entries {
		origin, err := ValidateCORSOrigin(entry)
		if err != nil {
			dropped = append(dropped, entry)
			continue
		}
		origins = append(origins, origin)
	}
	return origins, dropped
}
