// Command frontdoorhosts emits front-door manifests for each instance overlay
// from the cluster's role set and its domain.
//
// # Why generated
//
// The front door's standing rule is that its host count must not grow with
// customers, apps or sites (memql#3700), so the host set is a closed
// derivation rather than a list somebody maintains:
//
//	api         api.<d>
//	identity    identity.<d>
//	mcp         mcp.<d>
//	sites       portal.<d> (the platform's own site, site #1)
//	sites       *.<d> + the apex
//
// It emits ~440 lines of Ingress + Certificate from those six names, which is
// what earns generation for a listed target: hand-maintaining that is the same
// shape of mistake cmd/frontdoorpaths exists to stop, one level up. There, a
// path with no rule does not 404 -- it hands HTTP/1.1 to an h2c backend and
// fails naming nothing. Here, a host with no rule is a service that is simply
// not reachable at the name every client was told to dial -- and a host with a
// rule but no certificate SAN is one that answers with the ingress
// controller's self-signed default (memql#4224). The Certificate names every
// exact host and NO wildcard, because the cloud issuer is HTTP-01; see
// manifest.go's header for the decision and component/frontdoor for the
// derivation both sides share.
//
// This tool used to walk every overlay and take an ENVIRONMENT LABEL out of
// each one's environment.yaml, hyphenating it into role hosts and nesting it
// into site hosts. Epic memql#3943 removed "environment" as a product concept:
// there is one installation shape, and a second environment is a second
// install with its own domain and its own front door. Two instance overlays
// remain -- overlays/cloud (in-cluster Application, top) and
// overlays/cloud-entry (the entry / client install, memql#4204) -- and kustomize will
// not load a sibling generated file, so each overlay owns its own copy.
//
// # What is generated and what is not
//
// This tool owns the SHAPE and the HOSTS of each instance overlay's front
// door. It does NOT own the bff's HTTP path list -- cmd/frontdoorpaths does,
// from component/server's own declarations -- so the block between the markers
// in the api Ingress is PRESERVED across a regeneration and filled by
// `make frontdoor-paths`. `make frontdoor` runs both, in that order.
//
// The LOCAL overlay is deliberately not written here. It is traefik rather than
// nginx, and its four hand-authored front-door files carry the measured
// reasoning for the priority ranking that broke the API once already
// (memql#3810). What binds it to this derivation instead is a gate:
// deploy/k8s/overlays/frontdoor_hosts_test.go computes the host set from
// component/frontdoor and asserts the local render serves exactly it, so
// local's committed defaults cannot drift from it even though the file is
// written by hand.
//
// # Why the domain is a committed default rather than a real one
//
// No file under deploy/ names a real domain (memql#3593): the domain is a VALUE
// carried by the memql-domain ConfigMap, from which every node derives its
// hosts at boot. An Ingress host is a Kubernetes API object, so it has to be in
// the render anyway -- and the convention the local overlay already follows is
// to commit `memql.localhost` and let an install override it through the ArgoCD
// Application's spec.source.kustomize.patches. `.localhost` is unroutable
// externally, so a cluster reconciled before its domain is set fails visibly
// instead of half-working -- the same fail-closed placeholder discipline
// deploy/k8s/overlays/cloud already applies to its livekit address and its mcp
// digest.
//
// Usage:
//
//	go run ./cmd/frontdoorhosts             # write every instance overlay's front door
//	go run ./cmd/frontdoorhosts --check     # fail when either file is stale
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// generatedName is the file this tool owns in each instance overlay.
const generatedName = "front-door.generated.yaml"

// instanceOverlays is every overlay this tool writes into, relative to the
// overlays root. Listed rather than discovered: a walk that found zero
// overlays would generate nothing and exit 0 -- a generator reporting success
// without doing the work is the defect the --check gate exists to catch.
var instanceOverlays = []string{"cloud", "cloud-entry"}

// Markers, byte-identical to cmd/frontdoorpaths' -- the block between them is
// that tool's output and is carried across a regeneration here.
const (
	beginMarker = "          # BEGIN generated bff HTTP paths -- make frontdoor-paths"
	endMarker   = "          # END generated bff HTTP paths"
)

func main() {
	root := flag.String("overlays", filepath.Join("deploy", "k8s", "overlays"),
		"directory holding the kustomize overlays")
	domain := flag.String("domain", "memql.localhost",
		"the committed default domain -- an install overrides it through the ArgoCD Application's kustomize.patches")
	check := flag.Bool("check", false, "exit non-zero when a generated file is stale, writing nothing")
	flag.Parse()

	var stale []string
	for _, overlay := range instanceOverlays {
		dir := filepath.Join(*root, overlay)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			fail(fmt.Errorf("%s is not a directory, so there is no %s overlay to generate a "+
				"front door for", dir, overlay))
		}
		path := filepath.Join(dir, generatedName)

		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(fmt.Errorf("reading %s: %w", path, err))
		}
		want := manifest(overlay, *domain, preservedPathBlock(string(existing)))

		if bytes.Equal(existing, []byte(want)) {
			continue
		}
		if *check {
			stale = append(stale, path)
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			fail(fmt.Errorf("writing %s: %w", path, err))
		}
	}
	if *check && len(stale) > 0 {
		fail(fmt.Errorf("stale, run `make frontdoor-hosts`:\n  %s", strings.Join(stale, "\n  ")))
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	os.Exit(1)
}

// preservedPathBlock returns the bff HTTP path entries the existing file
// carries between cmd/frontdoorpaths' markers.
//
// Carried rather than regenerated because the two derivations answer different
// questions and must stay separable: this tool knows which HOSTS exist, and
// cmd/frontdoorpaths knows which PATHS the bff serves -- from component/server's
// own declarations, with an exhaustiveness gate that fails the build when a new
// declaration is classified by nothing. Re-deriving the paths here would be a
// second copy of that judgement, and the copy would be the one nobody updates.
//
// An empty result is correct for a file that does not exist yet:
// `make frontdoor-paths` fills it, and TestFrontDoorPathsAreNotStale fails
// until it has.
func preservedPathBlock(existing string) string {
	begin := strings.Index(existing, beginMarker)
	end := strings.Index(existing, endMarker)
	if begin < 0 || end < 0 || end < begin {
		return ""
	}
	return existing[begin+len(beginMarker)+1 : end]
}
