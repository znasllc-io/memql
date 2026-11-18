package edge

import "strings"

// csp_calls.go -- THE PAGE POLICY OF A DEPLOYABLE WHOSE CALLS SWITCH IS ON.
//
// A call's media never passes through MemQL: the page connects to a media server directly,
// for signalling over a WebSocket (wss) and for the connection check its client makes over
// https when signalling fails. Every hosted page is served connect-src naming its own origin
// and the cluster's identity, so without this a calls page's first connection is refused by
// its own policy before it reaches the network -- the routes answer, the pass is minted, and
// the call never connects. And the page has to be allowed to ask for the camera.
//
// BOTH ARE THE SWITCH'S, so a deployable without calls is served exactly the policy it was
// served before this file existed (TestCallsPagePolicyAdmitsOnlyTheMediaSuffix pins it byte
// for byte).
//
// # Which hosts
//
// MEMQL_CALLS_MEDIA_HOST_SUFFIX names the DNS name every media server of the installation is
// at or under (media.memql.localhost on a local cluster, media.<domain> for m1.media.<domain>
// in the cloud), and the policy admits that host and every host under it, over wss and https,
// on any port: a local media server answers on 7443, a cloud one on 443, and a policy that
// named a port would have to know which. A suffix rather than the list of media servers,
// because a media server is a row (v1:calls:mediaServer) that comes and goes at runtime while
// the policy is built per response from configuration, and reading rows on the path of every
// asset response would put a query on it. One value per installation, never a branch on which
// installation this is (CLAUDE.md "Environment parity").
//
// UNSET OR NOT A HOST NAME, the policy admits no media host -- the calls routes still answer
// -- and the edge says so once at boot (app/transport_edge.go). An installation without media
// servers is legal, and refusing to boot would take down every site this edge serves, so the
// failure is a refused media connection with one warning at boot as its clue.
//
// THE VALUE IS HELD TO A DNS NAME, and anything else drops it rather than reaching a response
// header: a scheme, a port, a path, a wildcard, an address, a space or a semicolon would each
// widen the policy or inject a directive. At least two labels, because a one-label suffix
// would admit everything under a top-level name.

// CallsMediaHostSuffixEnv names the variable. Exported for the boot check, which reads it
// with the same validation the policy uses, so the warning and the policy cannot disagree.
const CallsMediaHostSuffixEnv = "MEMQL_CALLS_MEDIA_HOST_SUFFIX"

// callsPermissionsPolicy is permissionsPolicy with the camera opened to the page's own
// document, which a video call needs: camera=() rejects getUserMedia before the browser
// prompts, which a page cannot tell from a person declining (csp.go says why that matters).
// Geolocation stays shut. The per-origin prompt still stands: this decides whether the page
// may ask, and the person decides whether it may look.
const callsPermissionsPolicy = "geolocation=(), microphone=(self), camera=(self)"

// ParseCallsMediaHostSuffix reads a suffix out of the variable's raw value: trimmed, lower
// case and without one final dot. ok is false for a value the policy may not name -- empty,
// or anything but a DNS name of at least two labels, each 1-63 letters, digits or hyphens not
// starting or ending with one, at most 253 characters in all, whose last label is not all
// digits (an address, not a name).
func ParseCallsMediaHostSuffix(raw string) (suffix string, ok bool) {
	suffix = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if suffix == "" || len(suffix) > 253 {
		return "", false
	}
	labels := strings.Split(suffix, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", false
	}
	return suffix, true
}

// callsMediaSources is what a calls page's connect-src gains: the suffix's own host and every
// host under it, over wss and https, on any port. "" for a site with calls off and for a
// suffix that is unset or not a host name.
func callsMediaSources(site *Site, env func(string) string) string {
	if site == nil || !site.Calls {
		return ""
	}
	suffix, ok := ParseCallsMediaHostSuffix(env(CallsMediaHostSuffixEnv))
	if !ok {
		return ""
	}
	return "wss://" + suffix + ":* wss://*." + suffix + ":* https://" + suffix + ":* https://*." + suffix + ":*"
}

// permissionsPolicyFor is the Permissions-Policy a site's responses carry: the camera open to
// a calls page and shut everywhere else.
func permissionsPolicyFor(site *Site) string {
	if site != nil && site.Calls {
		return callsPermissionsPolicy
	}
	return permissionsPolicy
}
