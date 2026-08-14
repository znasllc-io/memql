package local

import (
	"strings"
	"testing"
)

// render_priority_test.go -- memql#3810.
//
// THE REGRESSION. `traefik.ingress.kubernetes.io/router.priority: "100"` was
// added to api-front-door, front-door (identity) and mcp-front-door to make the
// three exact hosts outrank the `*.memql.localhost` wildcard, whose HostRegexp
// rule is LONGER than theirs and would otherwise win on traefik's default
// priority (= compiled rule length). That purpose was real and the fix worked
// for it: `precedence` verifies on a live cluster that identity.'s /healthz is
// answered by nodeType=identity rather than by the wildcard's nodeType=edge.
//
// But the annotation is INGRESS-level, and api-front-door declares TWENTY-TWO
// paths. One value applied to all of them flattened the ordering that had been
// doing quiet work WITHIN that host: rule length is what made
// Host(api…) && PathPrefix(/healthz) outrank Host(api…) && PathPrefix(/), and a
// uniform number removes the difference. So `/` -- the h2c gRPC catch-all
// pointed at bff:50051 -- could win over the 21 specific paths pointed at
// bff-http:8085, and did:
//
//	$ curl -sS -D- --http1.1 https://api.memql.localhost/healthz
//	HTTP/1.1 415 Unsupported Media Type
//	Content-Type: application/grpc
//	Grpc-Status: 3
//
// That is the memql#3703 failure exactly -- an HTTP/1.1 request handed to an
// h2c backend, failing with a protocol error that names nothing -- reached from
// the opposite direction. #3703 fixed a path having NO rule; this was a path
// having a rule that lost.
//
// Only api. broke, and the reason is the shape of the check: identity. and mcp.
// declare a single path each, so a uniform priority costs them nothing. That is
// also why verify-frontdoor.sh's `precedence` PASSED for identity. and went
// INCONCLUSIVE for api. -- and the inconclusive verdict, with its reason
// printed, is what pointed at this rather than at a wildcard problem.
//
// THE INVARIANT THIS PINS. A single Ingress-level priority cannot express both
// "beat the wildcard" and "keep my own paths in their relative order". So the
// ranking is declared in ONE place and the right one: the wildcard names itself
// LOWEST, and every exact host keeps traefik's length-based default. One
// annotation, on the Ingress whose rules are all a single shape, and no
// intra-host ordering is disturbed anywhere.

// frontDoorIngress is one Ingress from the rendered overlay, reduced to what
// this file reasons about.
type frontDoorIngress struct {
	name          string
	priority      string // "" when the annotation is absent
	hosts         []string
	distinctPaths map[string]bool
}

// TestNoMultiPathIngressDeclaresAUniformPriority is the regression gate.
//
// It is expressed as "how many DISTINCT paths does this Ingress declare", not
// "how many rules", because that is precisely the hazard. edge-front-door has
// two rules -- the wildcard and the apex -- but both are `/`, so there is no
// relative order among them for a uniform value to destroy. api-front-door had
// twenty-two different paths, one of which was a catch-all, and that is the
// configuration where a single number silently overrides length.
func TestNoMultiPathIngressDeclaresAUniformPriority(t *testing.T) {
	ingresses := frontDoorIngresses(t)

	for _, ing := range ingresses {
		if ing.priority == "" {
			continue
		}
		if len(ing.distinctPaths) > 1 {
			t.Errorf("Ingress %q declares router.priority=%s AND %d distinct paths (%s).\n\n"+
				"The annotation is Ingress-level: that one value applies to every router this "+
				"Ingress generates, so the paths lose the length-based ordering that makes a "+
				"specific PathPrefix outrank a catch-all `/`. On this Ingress that sent every "+
				"request declared for bff-http:8085 to the h2c gRPC backend bff:50051, which "+
				"answers HTTP/1.1 with `415 ... Content-Type: application/grpc` (memql#3810).\n\n"+
				"Declare precedence on the WILDCARD instead -- it has one path shape, so a "+
				"uniform value costs it nothing, and every exact host then keeps traefik's "+
				"default ordering.",
				ing.name, ing.priority, len(ing.distinctPaths), strings.Join(sortedKeys(ing.distinctPaths), " "))
		}
	}
}

// TestWildcardFrontDoorDeclaresItselfLowest pins the other half. Removing the
// annotation from api-front-door alone would fix the flattening and reopen what
// it was added for: the wildcard's HostRegexp rule is longer than the exact
// hosts' rules, so on pure length the wildcard can outrank them and the whole
// API gets served by the site edge.
//
// So SOMETHING must still declare the ranking. It is the wildcard, saying it
// loses -- one annotation, one direction, and no exact host's internal ordering
// is touched.
func TestWildcardFrontDoorDeclaresItselfLowest(t *testing.T) {
	ingresses := frontDoorIngresses(t)

	var wildcard *frontDoorIngress
	for i := range ingresses {
		for _, h := range ingresses[i].hosts {
			if strings.HasPrefix(h, "*.") {
				wildcard = &ingresses[i]
			}
		}
	}
	if wildcard == nil {
		t.Fatal("no Ingress in the rendered overlay declares a wildcard host, so this check " +
			"matched nothing and proved nothing")
	}

	if wildcard.priority == "" {
		t.Errorf("the wildcard Ingress %q declares no router.priority.\n"+
			"Its HostRegexp rule is LONGER than the exact hosts' rules, so on traefik's "+
			"default (priority = rule length) the wildcard can outrank api./identity./mcp. "+
			"and the whole API is served by the site edge. Something has to declare the "+
			"ranking; the wildcard declaring itself lowest is the form that does not disturb "+
			"any exact host's own path ordering (memql#3810).", wildcard.name)
	}

	// Every exact host must now rely on the default. A priority there is what
	// caused this, and re-adding one would flatten that host's paths again.
	for _, ing := range ingresses {
		if ing.name == wildcard.name {
			continue
		}
		if ing.priority != "" {
			t.Errorf("exact-host Ingress %q declares router.priority=%s.\n"+
				"With the wildcard declaring itself lowest, an exact host needs no priority of "+
				"its own -- and carrying one re-creates memql#3810 the moment that Ingress "+
				"grows a second path.", ing.name, ing.priority)
		}
	}
}

// frontDoorIngresses parses every Ingress out of the RENDERED overlay.
//
// Rendered, not read from the source files: the annotation reaching a given
// Ingress is a kustomize outcome, and the whole lesson of this class of bug is
// that the artifact under review is not the artifact that runs.
func frontDoorIngresses(t *testing.T) []frontDoorIngress {
	t.Helper()
	rendered := render(t)

	var out []frontDoorIngress
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if !strings.Contains(doc, "kind: Ingress") {
			continue
		}
		ing := frontDoorIngress{distinctPaths: map[string]bool{}}
		for _, line := range strings.Split(doc, "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "name:") && ing.name == "":
				ing.name = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			case strings.Contains(trimmed, "router.priority:"):
				v := strings.TrimSpace(trimmed[strings.Index(trimmed, "router.priority:")+len("router.priority:"):])
				ing.priority = strings.Trim(v, `"'`)
			case strings.HasPrefix(trimmed, "- host:"):
				ing.hosts = append(ing.hosts, strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- host:")), `"'`))
			case strings.HasPrefix(trimmed, "- path:"):
				ing.distinctPaths[strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- path:")), `"'`)] = true
			case strings.HasPrefix(trimmed, "path:"):
				ing.distinctPaths[strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "path:")), `"'`)] = true
			}
		}
		if len(ing.hosts) == 0 {
			continue // not a host-routed front door
		}
		out = append(out, ing)
	}

	if len(out) == 0 {
		t.Fatal("no host-routed Ingress found in the rendered overlay -- the scan matched " +
			"nothing, so every assertion built on it would pass vacuously")
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small sets; insertion order is not meaningful, so a simple sort keeps
	// failure messages stable across runs.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
