// Package overlays -- the reproducibility gates on every committed ArgoCD
// Application (epic memql#4463).
//
// WHY THESE EXIST. The first cloud instance was audited and found to be
// unreproducible from source, in two compounding ways that each looked healthy
// from the outside:
//
//  1. Its Application carried roughly TWENTY-FIVE inline kustomize patches --
//     the domain, image digest overrides, a workload-identity client id,
//     storage classes, certificate DNS names, every ingress host rewrite,
//     liveness-probe corrections, ExternalSecret deletions. None of it was in
//     git. The desired state of the installation existed only as a Kubernetes
//     object ON the cluster it configured, so the cluster was its own source of
//     truth and deleting it destroyed the specification.
//
//  2. Its `targetRevision` was `e070a0d` -- a bare commit SHA that DOES NOT
//     EXIST in this repository. Argo could not resolve its own target, which is
//     why the app sat permanently OutOfSync. A branch cannot vanish this way;
//     a squash-merged or force-pushed commit can, and did.
//
// Neither showed up as an outage. Argo reported `Healthy` throughout, because
// health describes the pods it last managed to apply, not whether the
// specification is still recoverable. That is what makes this a gate and not a
// review item: the failure mode is silent, indefinitely, until the day someone
// needs to rebuild.
//
// SCOPE, AND THE HALF THIS CANNOT SEE. These gates read the COMMITTED
// manifests, so they prevent the anti-pattern from entering the repository.
// The instance that failed was hand-edited live and had drifted away from the
// committed file, which no test of source can detect -- that is
// scripts/deploy/drift-check.sh's job, and the two are complements rather than
// substitutes.
package overlays

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// appsDir holds every committed ArgoCD Application.
const appsDir = "../../argocd/apps"

// bareSHA matches a git object name given as a revision: 7-40 hex characters
// and nothing else. A branch or tag never looks like this, and a tag would be
// acceptable anyway -- it is the UNNAMED commit that is unsafe, because nothing
// keeps it reachable.
var bareSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// applicationSource is the part of an Application these gates read. It is
// deliberately separate from render_cloud_test.go's argoApplication: that one
// models the fields a correct Application has, while this one models the fields
// a BROKEN one has, and a struct cannot usefully be both.
type applicationSource struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Source  *appSource  `yaml:"source"`
		Sources []appSource `yaml:"sources"`
	} `yaml:"spec"`
}

type appSource struct {
	RepoURL        string `yaml:"repoURL"`
	Path           string `yaml:"path"`
	TargetRevision string `yaml:"targetRevision"`
	Kustomize      *struct {
		// Patches is the field that carried an entire instance definition.
		Patches []map[string]any `yaml:"patches"`
		// Images is a digest pin expressed on the Application rather than in
		// the overlay. It is less severe than Patches -- a pin is one value,
		// not a specification -- but it is the same class of mistake and it is
		// how a live Application starts diverging from its committed form.
		Images []string `yaml:"images"`
	} `yaml:"kustomize"`
}

// committedApplications reads every Application manifest under deploy/argocd/apps.
func committedApplications(t *testing.T) map[string]applicationSource {
	t.Helper()

	entries, err := os.ReadDir(appsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", appsDir, err)
	}

	out := make(map[string]applicationSource)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(appsDir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		// A manifest file may hold several documents; only Applications matter.
		for _, doc := range strings.Split(string(b), "\n---") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var app applicationSource
			if err := yaml.Unmarshal([]byte(doc), &app); err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			if app.Kind != "Application" || app.Metadata.Name == "" {
				continue
			}
			out[e.Name()] = app
		}
	}

	if len(out) == 0 {
		t.Fatalf("no ArgoCD Applications found under %s -- this gate would pass vacuously", appsDir)
	}
	return out
}

func (a applicationSource) sources() []appSource {
	var out []appSource
	if a.Spec.Source != nil {
		out = append(out, *a.Spec.Source)
	}
	return append(out, a.Spec.Sources...)
}

// An Application says WHERE the desired state lives. The moment it also says
// WHAT that state is, the overlay stops being the specification and the
// Application becomes a second, unreviewable source of truth that lives on the
// cluster rather than in the repository.
func TestNoApplicationCarriesInlineKustomizePatches(t *testing.T) {
	for file, app := range committedApplications(t) {
		for _, src := range app.sources() {
			if src.Kustomize == nil {
				continue
			}
			if n := len(src.Kustomize.Patches); n > 0 {
				t.Errorf("%s (Application %q) carries %d inline kustomize patch(es).\n"+
					"An Application declares WHERE the desired state lives, never WHAT it is.\n"+
					"Move these into the overlay at %q, where they are reviewable in a diff and\n"+
					"survive the cluster being deleted. This is the defect that made the first\n"+
					"cloud instance unreproducible (memql#4463).",
					file, app.Metadata.Name, n, src.Path)
			}
			if n := len(src.Kustomize.Images); n > 0 {
				t.Errorf("%s (Application %q) pins %d image(s) on the Application.\n"+
					"Digest pins belong in the overlay's kustomization.yaml, which\n"+
					"scripts/deploy/pin-overlay-digests.sh already writes. A pin here is invisible\n"+
					"to every render gate and to code review.",
					file, app.Metadata.Name, n)
			}
		}
	}
}

// A bare SHA is not a durable reference. Squash-merging a branch, or
// force-pushing over it, removes the commit that was pinned -- and Argo then
// cannot resolve its own target while continuing to report the pods it applied
// last as Healthy.
func TestNoApplicationPinsABareCommitSHA(t *testing.T) {
	for file, app := range committedApplications(t) {
		for _, src := range app.sources() {
			rev := strings.TrimSpace(src.TargetRevision)
			if rev == "" {
				t.Errorf("%s (Application %q) declares no targetRevision -- Argo would silently track HEAD",
					file, app.Metadata.Name)
				continue
			}
			if bareSHA.MatchString(rev) {
				t.Errorf("%s (Application %q) pins targetRevision %q, a bare commit SHA.\n"+
					"Nothing keeps an unnamed commit reachable: a squash-merge or force-push\n"+
					"deletes it, after which Argo cannot resolve its target and reports\n"+
					"OutOfSync forever while still showing Healthy. Track a branch or a tag.\n"+
					"The first cloud instance was pinned to e070a0d, which no longer exists\n"+
					"in this repository (memql#4463).",
					file, app.Metadata.Name, rev)
			}
		}
	}
}

// The gates above are only worth as much as their reach: an Application that
// parses to zero sources is silently exempt from both.
func TestEveryApplicationDeclaresAResolvableSource(t *testing.T) {
	for file, app := range committedApplications(t) {
		srcs := app.sources()
		if len(srcs) == 0 {
			t.Errorf("%s (Application %q) declares neither spec.source nor spec.sources -- "+
				"it would be exempt from every reproducibility gate in this file",
				file, app.Metadata.Name)
			continue
		}
		for _, src := range srcs {
			if strings.TrimSpace(src.RepoURL) == "" {
				t.Errorf("%s (Application %q) declares a source with no repoURL", file, app.Metadata.Name)
			}
			if strings.TrimSpace(src.Path) == "" && len(srcs) == 1 {
				t.Errorf("%s (Application %q) declares a source with no path", file, app.Metadata.Name)
			}
		}
	}
}
