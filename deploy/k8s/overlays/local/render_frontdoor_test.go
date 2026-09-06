// Package local — see render_domain_test.go for why these are tests and not
// reviews. This file covers the front door itself: which hosts it serves, and
// which entrances no longer exist.
package local

import (
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// The six hosts the front door serves (design D3, plus the OS shell's own exact
// rule from memql#4224). The COUNT is the invariant: it must not grow with
// customers, apps or sites.
//
// COMPUTED from component/frontdoor rather than listed, since memql#3767. Local
// is ONE environment (TestLocalStaysOneEnvironment) and it is the UNPREFIXED
// one, so its host set is exactly what the derivation yields for the committed
// default domain -- the same set cmd/frontdoorhosts writes into the two cloud
// overlays.
//
// The point of computing it is that this overlay's five front-door files stay
// HAND-AUTHORED, and deliberately so: they are traefik rather than nginx, and
// they carry the measured reasoning for a priority ranking that broke the API
// once already (memql#3810). Hand-authored is not the same as unchecked. This
// binds them to the same derivation the generator uses, so local's committed
// defaults cannot drift from what the cloud overlay serves -- which is what
// would make the local cluster stop proving anything about the cloud one.
//
// The OS shell is the host this gate most needs to keep honest: locally the
// mkcert wildcard covers os.memql.localhost whether or not an exact rule
// exists, so a developer would never notice the rule missing. In the cloud the
// certificate names exact hosts only (HTTP-01 cannot issue a wildcard), and
// without the rule the OS serves the ingress controller's self-signed default.
// Same six rules everywhere is what lets local prove the shape. (It was the
// portal's rule that taught this, and the portal was retired in epic
// memql#4984 -- the OS inherited the exception whole, and TestTheOsRuleReachesTheEdge
// below is the portal test renamed rather than a new one.)
var frontDoorHosts = func() []string {
	var out []string
	for _, h := range frontdoor.Hosts("memql.localhost") {
		out = append(out, h.Name)
	}
	return out
}()

// hostsIn returns every Ingress rule host in the rendered overlay. Parsed by
// line rather than with a YAML decoder because the rendered stream is many
// documents of many kinds, and the only thing wanted is `host:` under
// spec.rules — which is unambiguous at the text level and needs no schema.
func hostsIn(rendered string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(rendered, "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "- ")
		if !strings.HasPrefix(t, "host: ") {
			continue
		}
		h := strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "host: ")), `"'`)
		if h != "" {
			out[h] = true
		}
	}
	return out
}

func TestFrontDoorServesExactlyTheDerivedHosts(t *testing.T) {
	got := hostsIn(render(t))

	for _, want := range frontDoorHosts {
		if !got[want] {
			t.Errorf("front door does not serve %q", want)
		}
	}
	if len(got) != len(frontDoorHosts) {
		var extra []string
		want := map[string]bool{}
		for _, h := range frontDoorHosts {
			want[h] = true
		}
		for h := range got {
			if !want[h] {
				extra = append(extra, h)
			}
		}
		sort.Strings(extra)
		t.Errorf("front door serves %d hosts, want %d; unexpected: %v",
			len(got), len(frontDoorHosts), extra)
	}
}

// D4: the endpoint is named for its role. Pre-release, so there is no alias
// and no redirect — the old name is simply gone.
func TestCockpitHostIsGone(t *testing.T) {
	if strings.Contains(render(t), "cockpit.") {
		t.Error("the rendered overlay still names a cockpit. host; D4 renamed it to api.")
	}
}

// A second entrance is a connection path that exists in one environment and
// not the others, which is what environment-parity.md forbids. identity was
// reachable both through the front door and directly on host port 8085.
func TestNoSecondEntranceToIdentity(t *testing.T) {
	if strings.Contains(render(t), "identity-external") {
		t.Error("identity-external still exists; identity is reachable only through the front door")
	}
}

func TestTheOsRuleReachesTheEdge(t *testing.T) {
	rendered := render(t)
	osHost := frontdoor.OsHost("memql.localhost")

	var found bool
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if !strings.Contains(doc, "kind: Ingress") || !strings.Contains(doc, "host: "+osHost) {
			continue
		}
		found = true
		if !strings.Contains(doc, "name: edge") {
			t.Errorf("the Ingress serving %q does not point at svc/edge; the OS is a site and takes the site path", osHost)
		}
		if strings.Contains(doc, "router.priority") {
			t.Errorf("the Ingress serving %q declares a router.priority; precedence is declared on the wildcard only (memql#3810)", osHost)
		}
	}
	if !found {
		t.Fatalf("no Ingress in the rendered overlay carries an exact rule for %q", osHost)
	}
}
