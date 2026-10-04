package packages

import (
	"fmt"

	"github.com/znasllc-io/memql/component/edge"
)

// candidate.go -- a package deploy that publishes a deployable as its
// CANDIDATE version (memql#5601).
//
// The seam is component/edge's: Publisher.Publish takes a target, and every
// SiteStore issues edge.PointVersionStatement, so a candidate publish writes
// candidateRef and never bundleRef whichever route made it. What this file
// adds is the package half of the question -- which runs may ask for one, and
// what a run that did has deployed.
//
// THE TWO REFUSALS BELOW ARE PLAIN ERRORS, NOT CATALOGUE CODES. Every code in
// refusal.go needs MemQL OS copy in the same change (refusals.test.ts reads
// this package and fails on a code with no home), and the copy lives in a
// surface this change does not edit. A plain error closes the run with the
// pipeline's own deploy_failed and this sentence, which the OS already
// renders; promoting either to a code is a change that adds its copy too.

// placementTargets reads every placement's target into one of edge's two,
// refusing an unknown value. It returns a NEW map, because the request's map
// belongs to its caller.
func placementTargets(placements map[string]Placement) (map[string]Placement, error) {
	if placements == nil {
		return nil, nil
	}
	out := make(map[string]Placement, len(placements))
	for name, p := range placements {
		target, err := edge.ParseTarget(string(p.Target))
		if err != nil {
			return nil, fmt.Errorf("placements[%q]: %w", name, err)
		}
		p.Target = target
		out[name] = p
	}
	return out, nil
}

// refuseUnservedCandidates refuses a run that asks for a candidate where no
// candidate is served.
//
// A STOREFRONT HAS NO CANDIDATE VERSION. It has two destinations over ONE
// published build: Production at its own hostname and Testing at its test--
// alias, each against its own store (docs/public/operate/storefront-preview.md).
// The edge serves bundleRef on both, so a storefront candidate would be
// served by nothing -- the run would report a publish and the build would be
// visible nowhere. The engine refuses a storefront candidate that is not the
// serving version as well (component/memql/platform_site_preview_guard.go);
// this is the same answer one step earlier, for every storefront candidate
// placement, before a byte is built or a binding re-pointed.
//
// A skipped deployable is not judged: it publishes nothing.
func refuseUnservedCandidates(rep *Report, placements map[string]Placement) error {
	if rep == nil {
		return nil
	}
	for _, dep := range rep.Deployables {
		p := placements[dep.Name]
		if p.Skip || p.Target != edge.TargetCandidate || dep.Kind != KindStorefront {
			continue
		}
		return fmt.Errorf(
			"deployable %q is a shopify_storefront, and a storefront has no candidate version: its Testing destination serves the published build against the testing store, so a candidate would be served by nothing. Publish it as the serving version, which reaches Testing and Production together, or leave it out of this run",
			dep.Name)
	}
	return nil
}

// candidatesOnly reports whether a run's publishes were all candidates -- at
// least one candidate, and no serving version. Such a run changed nothing a
// visitor is served.
func candidatesOnly(outcomes []DeployableOutcome) bool {
	candidates := 0
	for _, o := range outcomes {
		if o.BundleRef != "" {
			return false
		}
		if o.CandidateRef != "" {
			candidates++
		}
	}
	return candidates > 0
}
