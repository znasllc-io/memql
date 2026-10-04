package packages

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

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

// placementTargets checks every placement's target, refusing an unknown value.
// It returns a NEW map, because the request's map belongs to its caller.
//
// AN OMITTED TARGET STAYS OMITTED (memql#5601). It is the serving version for
// publishing, but it is not the same answer as an explicit "serving": at the
// confirm gate an omitted target keeps what the run recorded when it opened,
// and an explicit one replaces it (withRecordedCandidates).
func placementTargets(placements map[string]Placement) (map[string]Placement, error) {
	if placements == nil {
		return nil, nil
	}
	out := make(map[string]Placement, len(placements))
	for name, p := range placements {
		if p.Target != "" {
			target, err := edge.ParseTarget(string(p.Target))
			if err != nil {
				return nil, fmt.Errorf("placements[%q]: %w", name, err)
			}
			p.Target = target
		}
		out[name] = p
	}
	return out, nil
}

// withRecordedCandidates gives every recorded candidate whose placement names
// no target back its candidate target. An explicit target is left alone:
// that is the person's answer at the gate. It returns a NEW map.
func withRecordedCandidates(placements map[string]Placement, recorded []string) map[string]Placement {
	if len(recorded) == 0 {
		return placements
	}
	out := make(map[string]Placement, len(placements)+len(recorded))
	for name, p := range placements {
		out[name] = p
	}
	for _, name := range recorded {
		p := out[name]
		if p.Target == "" {
			p.Target = edge.TargetCandidate
			out[name] = p
		}
	}
	return out
}

// candidateNames is the deployables placements publish as candidates, sorted,
// so the value recorded on the run has one spelling. A skipped app publishes
// nothing, so it is not one.
func candidateNames(placements map[string]Placement) []string {
	out := []string{}
	for name, p := range placements {
		if p.Target == edge.TargetCandidate && !p.Skip {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// sameNames reports whether two name lists hold the same names.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, n := range a {
		seen[n] = true
	}
	for _, n := range b {
		if !seen[n] {
			return false
		}
	}
	return true
}

// openRun opens a NEW run, recording the candidates its placements name.
//
// A RETRY KEEPS THE LOST RUN'S CANDIDATES. A retry restarts the run that was
// lost, from its own bytes, so it publishes the way that run would have: an
// omitted target takes the earlier run's recorded one, exactly as a confirm
// does at the gate. An earlier run that cannot be read, or that belongs to
// another package, contributes nothing here -- the fetch refuses the retry on
// its own terms.
func (d *Deps) openRun(ctx context.Context, req *DeployRequest, seed deploymentSeed) error {
	if from := strings.TrimSpace(req.FromDeploymentId); from != "" {
		prior, err := d.Store.deploymentById(ctx, from)
		if err != nil {
			return err
		}
		if prior != nil && sameShortId(rowString(prior, "packageId"), req.PackageId) {
			req.Placements = withRecordedCandidates(req.Placements, rowStrings(prior, "candidates"))
		}
	}
	seed.Candidates = candidateNames(req.Placements)
	return d.Store.openDeployment(ctx, seed)
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

// refuseCandidateDslChange refuses a run that publishes any deployable as its
// candidate while carrying MemQL DSL that differs from what the cluster runs.
//
// THE DSL HALF OF A RUN IS NEVER A CANDIDATE. Staging a changed domain flips
// the active-set pointer and rolls every node that reads DSL, for every
// visitor at once, while a candidate leaves every visitor on the serving
// build -- so the public would keep app N running against DSL N+1, half a
// version nobody chose, and the run would return without recording the
// source as deployed. The question is answered with the stager's own
// PrefixFor against the pointer as it stands, so "changed" means what the
// stage would mean by it, and nothing is written to ask.
//
// A cluster with no stager is not judged here: such a run cannot stage at
// all, and stageAndRoll refuses it before anything is published.
func (d *Deps) refuseCandidateDslChange(ctx context.Context, snapshot *SourceSnapshot, rep *Report, placements map[string]Placement) error {
	if rep == nil || snapshot == nil || len(rep.DslDomains) == 0 || d.Stager == nil {
		return nil
	}
	var candidates []string
	for _, dep := range rep.Deployables {
		if p := placements[dep.Name]; !p.Skip && p.Target == edge.TargetCandidate {
			candidates = append(candidates, fmt.Sprintf("%q", dep.Name))
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	current, err := d.Stager.ReadActiveSet(ctx)
	if err != nil {
		return err
	}
	var changed []string
	for _, domain := range rep.DslDomains {
		sub, err := fs.Sub(snapshot.Tree, path.Join(DslRoot, domain.Domain))
		if err != nil {
			return err
		}
		prefix, err := d.Stager.PrefixFor(domain.Domain, sub)
		if err != nil {
			return err
		}
		if current[domain.Domain] != prefix {
			changed = append(changed, domain.Domain)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	return fmt.Errorf(
		"this run publishes %s as a candidate, and it also carries MemQL DSL that differs from what this cluster runs (%s). Staging that DSL would restart every node onto it while the public keeps the serving build, so a candidate run cannot carry a DSL change. Publish this run to the serving version instead, or deploy the DSL change first -- a run with every app skipped stages and rolls it and publishes nothing -- and then publish the candidate, whose DSL will match",
		strings.Join(candidates, ", "), strings.Join(changed, ", "))
}
