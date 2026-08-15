// Command frontdoorhosts emits each cloud environment's front-door manifests
// from the product of ROLE and ENVIRONMENT.
//
// # Why generated
//
// One cluster now carries two environments in two namespaces (epic memql#3748 /
// memql#3766), and the front door's standing rule is that its host count must
// not grow with customers, apps or sites (memql#3700). Environment is none of
// those -- it is a closed set the operator chooses -- so the host set becomes a
// PRODUCT rather than a list somebody maintains:
//
//	role     x  unprefixed        x  labelled
//	api         api.<d>              api-<label>.<d>
//	identity    identity.<d>         identity-<label>.<d>
//	mcp         mcp.<d>              mcp-<label>.<d>
//	sites       *.<d> + the apex     *.<label>.<d>
//
// Hand-maintaining that product is the same shape of mistake cmd/frontdoorpaths
// exists to stop, one level up: there, a path with no rule does not 404, it
// hands HTTP/1.1 to an h2c backend and fails naming nothing. Here, a host with
// no rule in ONE of the two environments is an environment that is simply not
// reachable, and an operator debugging it is looking at a manifest that appears
// complete because the other environment's copy is right there beside it.
//
// # What is generated and what is not
//
// This tool owns the SHAPE and the HOSTS of the two cloud overlays' front
// doors. It does NOT own the bff's HTTP path list -- cmd/frontdoorpaths does,
// from component/server's own declarations -- so the block between the markers
// in the api Ingress is PRESERVED across a regeneration and filled by
// `make frontdoor-paths`. `make frontdoor` runs both, in that order.
//
// The LOCAL overlay is deliberately not written here. Local is ONE environment
// (TestLocalStaysOneEnvironment), it is traefik rather than nginx, and its four
// hand-authored front-door files carry the measured reasoning for the priority
// ranking that broke the API once already (memql#3810). What binds it to this
// product instead is a gate: deploy/k8s/overlays/frontdoor_hosts_test.go
// computes the unprefixed host set from component/frontdoor and asserts the
// local render serves exactly it, so local's committed defaults cannot drift
// from the product even though the file is written by hand.
//
// # Why the domain is a committed default rather than a real one
//
// No file under deploy/ names a real domain (memql#3593): the domain is a VALUE
// carried by the memql-domain ConfigMap, from which every node derives its
// hosts at boot. An Ingress host is a Kubernetes API object, so it has to be in
// the render anyway -- and the convention the local overlay already follows is
// to commit `memql.localhost` and let an install override it through the ArgoCD
// Application's spec.source.kustomize.patches. `.localhost` is unroutable
// externally, so an environment reconciled before its domain is set fails
// visibly instead of half-working -- the same fail-closed placeholder discipline
// deploy/k8s/overlays/prod already applies to its livekit address and its mcp
// digest.
//
// Usage:
//
//	go run ./cmd/frontdoorhosts             # write every cloud overlay
//	go run ./cmd/frontdoorhosts --check     # fail when one is stale
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// generatedName is the file this tool owns in each cloud overlay.
const generatedName = "front-door.generated.yaml"

// environmentFile is the overlay's environment VALUES -- the file memql#3766
// created, and the one that decides whether an overlay is a cloud environment
// at all. An overlay without it is a single-environment cluster (every local
// one) and is not written here.
const environmentFile = "environment.yaml"

// hostLabelKey is the third value on the memql-environment ConfigMap, beside
// MEMQL_ENVIRONMENT and MEMQL_DB_SEARCH_PATH. Empty means the unprefixed
// environment.
//
// It is read from the overlay rather than declared here on purpose: that is
// what makes a third environment a VALUE change. Adding one is a new overlay
// directory with a new label in it, and this tool generates its front door with
// no edit of its own -- which is the acceptance criterion memql#3767 states and
// TestAThirdEnvironmentIsAValueChange proves by rendering one.
const hostLabelKey = "MEMQL_HOST_LABEL"

// environmentKey names the environment, used only in the generated header so a
// reader of the manifest knows which overlay they are in.
const environmentKey = "MEMQL_ENVIRONMENT"

// Markers, byte-identical to cmd/frontdoorpaths' -- the block between them is
// that tool's output and is carried across a regeneration here.
const (
	beginMarker = "          # BEGIN generated bff HTTP paths -- make frontdoor-paths"
	endMarker   = "          # END generated bff HTTP paths"
)

// environment is one overlay, reduced to what the front door needs.
type environment struct {
	dir   string // overlay directory name
	path  string // the generated file's path
	name  string // MEMQL_ENVIRONMENT, for the header
	label string // MEMQL_HOST_LABEL, "" for the unprefixed environment
}

// configMap is the slice of environment.yaml this tool reads.
type configMap struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

// discover finds every cloud overlay under root.
//
// DISCOVERED, not listed -- the one place in this tree where that is the right
// call. Everywhere else (the node list in the render gates, the environment
// table in environments_test.go) a list is correct because the failure worth
// catching is a new member arriving and nobody DECIDING about it. Here the
// deciding is the point: a third environment is meant to be a directory with a
// label in it and nothing else, so a list would be exactly the hand-maintained
// thing this tool exists to remove.
func discover(root string) ([]environment, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}

	var out []environment
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		envPath := filepath.Join(root, e.Name(), environmentFile)
		raw, err := os.ReadFile(envPath)
		if errors.Is(err, os.ErrNotExist) {
			continue // a single-environment overlay; not ours to write
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", envPath, err)
		}

		var cm configMap
		if err := yaml.Unmarshal(raw, &cm); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", envPath, err)
		}
		label := frontdoor.NormalizeLabel(cm.Data[hostLabelKey])
		if _, declared := cm.Data[hostLabelKey]; !declared {
			return nil, fmt.Errorf("%s declares no %s: an environment with a namespace and a "+
				"schema but no host label has data isolation and no front door, which is an "+
				"environment nothing can reach. Declare it -- empty means the unprefixed "+
				"environment, and exactly one environment may be that", envPath, hostLabelKey)
		}
		if err := frontdoor.ValidateLabel(label); err != nil {
			return nil, fmt.Errorf("%s: %w", envPath, err)
		}

		out = append(out, environment{
			dir:   e.Name(),
			path:  filepath.Join(root, e.Name(), generatedName),
			name:  strings.TrimSpace(cm.Data[environmentKey]),
			label: label,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })

	// Two environments claiming the same label would generate two Ingress rules
	// for one host in two namespaces, which resolves by whichever the
	// controller saw first -- a coin flip between environments, silently.
	byLabel := map[string]string{}
	for _, e := range out {
		if prior, dup := byLabel[e.label]; dup {
			return nil, fmt.Errorf("overlays %s and %s both use host label %q, so they would "+
				"claim the same hostnames in two namespaces and the ingress controller would "+
				"resolve it by whichever it saw first", prior, e.dir, e.label)
		}
		byLabel[e.label] = e.dir
	}
	return out, nil
}

func main() {
	root := flag.String("overlays", filepath.Join("deploy", "k8s", "overlays"),
		"directory holding the environment overlays")
	domain := flag.String("domain", "memql.localhost",
		"the committed default domain -- an install overrides it through the ArgoCD Application's kustomize.patches")
	check := flag.Bool("check", false, "exit non-zero when a generated file is stale, writing nothing")
	flag.Parse()

	envs, err := discover(*root)
	if err != nil {
		fail(err)
	}
	if len(envs) == 0 {
		fail(fmt.Errorf("no overlay under %s carries an %s, so there is no cloud environment to "+
			"generate a front door for", *root, environmentFile))
	}

	var stale []string
	for _, env := range envs {
		existing, err := os.ReadFile(env.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(fmt.Errorf("reading %s: %w", env.path, err))
		}
		want := manifest(env, *domain, preservedPathBlock(string(existing)))

		if bytes.Equal(existing, []byte(want)) {
			continue
		}
		if *check {
			stale = append(stale, env.path)
			continue
		}
		if err := os.WriteFile(env.path, []byte(want), 0o644); err != nil {
			fail(fmt.Errorf("writing %s: %w", env.path, err))
		}
	}

	if len(stale) > 0 {
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
