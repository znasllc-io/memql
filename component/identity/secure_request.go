package identity

import (
	"net/http"
	"os"
	"strings"
)

// RequestIsSecure reports whether a request reached the binary over TLS,
// either directly or through a reverse proxy that terminated it.
//
// EXTRACTED, NOT DUPLICATED (memql#3408). This predicate was written for the
// worker-pairing endpoints, which carry a plaintext credential in a header and
// in a JSON body. The enrolment link (memql#3408) is the same threat shape --
// a plaintext bearer, this time in a URL -- and the second surface to need the
// check is exactly the moment to stop having one copy of it. A second
// hand-written version is how the X-Forwarded-Proto arm ends up present on one
// endpoint and missing on the other, which is a deployment-shaped bug nobody
// notices until a proxy is in front.
//
// The X-Forwarded-Proto arm is load-bearing rather than lenient: in every
// environment memQL actually runs in, TLS terminates at the ingress and the
// binary sees plaintext on the hop behind it. Without this arm the check would
// refuse every production request and admit only the ones nobody makes.
//
// Note what this does NOT do: it makes no authorization decision from the
// header. A forged X-Forwarded-Proto buys an attacker the right to send a
// request over a channel they already control. The header describes the
// deployment's posture; it is not a credential.
func RequestIsSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// InsecureTransportEscapeEnabled reports whether the named dev-only env var is
// set to "1".
//
// Each surface names its OWN variable rather than sharing one. The escape
// hatch is per-credential on purpose: an operator debugging worker pairing on
// a laptop should not thereby be admitting plaintext enrolment links, and a
// single global switch is the shape that makes that happen by accident.
//
// Callers are expected to log loudly when this returns true -- an accidental
// production toggle has to be visible in a log shipper, not merely permitted.
func InsecureTransportEscapeEnabled(envVar string) bool {
	return strings.TrimSpace(os.Getenv(envVar)) == "1"
}
