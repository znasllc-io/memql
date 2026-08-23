package deploycontrol

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The read call sites, verbatim from repair.go and service.go. If one of those
// changes shape, this table is where the in-cluster substrate finds out --
// parseKubectlGet is strict on purpose, so an unrecognised shape is an error
// rather than a quietly different request.
func TestParseKubectlGetCoversEveryReadCallSite(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantNS   string
		wantRes  string
		wantName string
		wantPath string
	}{
		{
			name:     "argo application, by name (repair.go + service.go)",
			args:     []string{"-n", "argocd", "get", "app", "memql", "-o", "json"},
			wantNS:   "argocd",
			wantRes:  "app",
			wantName: "memql",
			wantPath: "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql",
		},
		{
			name:     "rollouts collection (service.go)",
			args:     []string{"-n", "memql", "get", "rollout", "-o", "json"},
			wantNS:   "memql",
			wantRes:  "rollout",
			wantPath: "apis/argoproj.io/v1alpha1/namespaces/memql/rollouts",
		},
		{
			name:     "analysisruns collection (service.go)",
			args:     []string{"-n", "memql", "get", "analysisrun", "-o", "json"},
			wantNS:   "memql",
			wantRes:  "analysisrun",
			wantPath: "apis/argoproj.io/v1alpha1/namespaces/memql/analysisruns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, verb, res, name, err := parseKubectlGet(tc.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if verb != "get" {
				t.Errorf("verb = %q, want get", verb)
			}
			if ns != tc.wantNS || res != tc.wantRes || name != tc.wantName {
				t.Errorf("got (ns=%q res=%q name=%q), want (ns=%q res=%q name=%q)",
					ns, res, name, tc.wantNS, tc.wantRes, tc.wantName)
			}
			plural, ok := kubectlPlural[res]
			if !ok {
				t.Fatalf("resource %q has no plural mapping, so the Role cannot grant it", res)
			}
			if got := argoPath(ns, plural, name); got != tc.wantPath {
				t.Errorf("path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// The strictness is the feature. A lenient parser would reinterpret a call it
// did not understand as a different, VALID one -- the wrong namespace, or a
// collection where a single object was meant -- and a deploy console reporting
// another namespace's state is worse than one that errors.
func TestParseKubectlGetRefusesWhatItCannotServeExactly(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no namespace", []string{"get", "app", "memql", "-o", "json"}, "names no namespace"},
		{"non-json output", []string{"-n", "argocd", "get", "app", "-o", "yaml"}, "only -o json"},
		{"flag with no value", []string{"-n"}, "ends after -n"},
		{"too few positionals", []string{"-n", "memql", "get"}, "cannot read"},
		{"too many positionals", []string{"-n", "memql", "get", "app", "memql", "extra"}, "cannot read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := parseKubectlGet(tc.args); err == nil {
				t.Fatalf("parsed %v without error; it must refuse rather than guess", tc.args)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A resource outside the closed map must be refused by NAME, because the Role
// grants three kinds and reaching a fourth has to be a compile-time decision
// with a matching rule -- not a string that happens to route.
func TestInClusterKubectlRefusesUngrantedResources(t *testing.T) {
	e := &inClusterExecutor{}
	for _, res := range []string{"secret", "pod", "configmap", "appproject"} {
		_, err := e.KubectlJSON(context.Background(), "-n", "memql", "get", res, "-o", "json")
		if err == nil {
			t.Errorf("%s was served; the substrate must refuse a resource its Role does not grant", res)
			continue
		}
		if !strings.Contains(err.Error(), "not one this node's Role grants") {
			t.Errorf("%s: error %q does not say the Role does not grant it", res, err)
		}
	}
}

// Both impossible verbs must name the missing prerequisite. The whole point of
// memql#4257 is that they answered "kickoff_failed", which sent operators
// looking at ArgoCD when the actual absence was a binary or a checkout.
func TestInClusterRefusalsNameTheMissingPrerequisite(t *testing.T) {
	e := &inClusterExecutor{}

	if _, err := e.RunRollback(context.Background(), "abcdef1234567890"); err == nil {
		t.Error("RunRollback succeeded with no checkout")
	} else {
		msg := err.Error()
		if !strings.HasPrefix(msg, ReasonNoOverlayCheckout+":") {
			t.Errorf("rollback refusal does not lead with %q: %s", ReasonNoOverlayCheckout, msg)
		}
		for _, want := range []string{"deploy checkout", "distroless", "memql#4275"} {
			if !strings.Contains(msg, want) {
				t.Errorf("rollback refusal omits %q: %s", want, msg)
			}
		}
	}

	if _, err := e.RunRolloutAction(context.Background(), "bff", "promote"); err == nil {
		t.Error("RunRolloutAction succeeded with no kubectl plugin")
	} else {
		msg := err.Error()
		if !strings.HasPrefix(msg, ReasonNoRolloutPlugin+":") {
			t.Errorf("rollout refusal does not lead with %q: %s", ReasonNoRolloutPlugin, msg)
		}
		if !strings.Contains(msg, "plugin") {
			t.Errorf("rollout refusal does not say it is a plugin: %s", msg)
		}
	}
}

// The refusal distinguishes "no repo root configured" from "a repo root that
// holds nothing", because they ask the operator for different next steps.
func TestNoCheckoutRefusalDistinguishesUnsetFromEmpty(t *testing.T) {
	unset := (&inClusterExecutor{}).noCheckout("x").Error()
	if !strings.Contains(unset, "MEMQL_DEPLOY_REPO_ROOT is unset") {
		t.Errorf("unset case does not say so: %s", unset)
	}
	set := (&inClusterExecutor{repoRoot: "/app"}).noCheckout("x").Error()
	if !strings.Contains(set, "MEMQL_DEPLOY_REPO_ROOT is /app, which holds no repository") {
		t.Errorf("set case does not name the path: %s", set)
	}
}

// InClusterAvailable needs BOTH halves, and neither implies the other:
// KUBERNETES_SERVICE_* is injected into every pod including ones with
// automountServiceAccountToken false, while the token file exists only where a
// ServiceAccount is actually projected. Choosing the substrate on the address
// alone builds a client that 401s on its first call.
func TestInClusterAvailableNeedsBothHalves(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if InClusterAvailable() {
		t.Error("reported available with no API-server address")
	}
	// With an address but (on any developer machine or CI runner) no projected
	// token, it must still be false. Guard the assertion so it says something
	// on the one host where the path does exist.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if _, err := os.Stat(saTokenPath); err != nil && InClusterAvailable() {
		t.Error("reported available with an address but no ServiceAccount token")
	}
}

// TestArgoPathIsNamespacedAndVersioned pins the one string the Role's rules
// have to agree with. A path that dropped the namespace would read across the
// cluster, and the Role would refuse it -- as a 403 rather than as this test.
func TestArgoPathIsNamespacedAndVersioned(t *testing.T) {
	got := argoPath("argocd", "applications", "memql")
	const want = "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"
	if got != want {
		t.Fatalf("argoPath = %q, want %q", got, want)
	}
	if coll := argoPath("memql", "rollouts", ""); strings.HasSuffix(coll, "/") {
		t.Errorf("collection path has a trailing slash: %q", coll)
	}
}
