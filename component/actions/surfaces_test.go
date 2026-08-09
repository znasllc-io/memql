package actions

import (
	"testing"

	"github.com/znasllc-io/memql/component/harness/surfaceresolver"
)

// surfaceBySlug is a tiny test helper.
func surfaceBySlug(surfaces []surfaceresolver.Surface, slug string) (surfaceresolver.Surface, bool) {
	for _, s := range surfaces {
		if s.Slug == slug {
			return s, true
		}
	}
	return surfaceresolver.Surface{}, false
}

// TestBuiltinSurfaces_CockpitRunnerServesDeployCapabilities asserts the
// cockpit/runner control surface is registered, available, and serves the
// deploy/control capability namespaces (via the resolver's wildcard match) so
// deploy actions resolve to it.
func TestBuiltinSurfaces_CockpitRunnerServesDeployCapabilities(t *testing.T) {
	surfaces := BuiltinSurfaces()
	cr, ok := surfaceBySlug(surfaces, CockpitRunnerSurface)
	if !ok {
		t.Fatalf("cockpit/runner surface not registered; got %+v", surfaces)
	}
	if !cr.Available {
		t.Fatalf("cockpit/runner must be available by default")
	}
	if cr.Kind != "cockpit-runner" {
		t.Errorf("kind = %q, want cockpit-runner", cr.Kind)
	}

	// The resolver must bind every representative deploy capability to the
	// cockpit/runner surface (it is the only registered surface that serves
	// them), proving a deploy action runs outside the target cluster.
	for _, capability := range []string{
		"shell.exec",
		"fs.writeFile",
		"fs.readFile",
		"http.get",
		"integration.argocd.sync",
		"integration.github.tagRelease",
	} {
		got, err := surfaceresolver.Resolve(capability, "", "", surfaces)
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error %v", capability, err)
			continue
		}
		if got.Slug != CockpitRunnerSurface {
			t.Errorf("Resolve(%q) = %q, want %q", capability, got.Slug, CockpitRunnerSurface)
		}
	}
}

// TestBuiltinSurfaces_DoesNotServeUnlistedNamespace guards the wildcard match:
// a capability outside the declared namespaces is NOT served by cockpit/runner.
func TestBuiltinSurfaces_DoesNotServeUnlistedNamespace(t *testing.T) {
	surfaces := BuiltinSurfaces()
	if _, err := surfaceresolver.Resolve("mcp.invoke", "", "", surfaces); err == nil {
		t.Fatalf("cockpit/runner must NOT serve mcp.* (carrier surface territory)")
	}
}

// TestPolicyDefaultSurface_DeployPackInheritsCockpitRunner pins the policy-
// default mechanism: deployment-pack actions (by DSL-tree origin) and control-
// plane capabilities default to cockpit/runner; ordinary actions do not.
func TestPolicyDefaultSurface_DeployPackInheritsCockpitRunner(t *testing.T) {
	cases := []struct {
		name   string
		action *Action
		want   string
	}{
		{
			name:   "deploy-pack origin inherits cockpit/runner",
			action: &Action{Capability: "shell.exec", Origin: "deployment/actions.memql:syncArgoApp"},
			want:   CockpitRunnerSurface,
		},
		{
			name:   "deploy/ origin inherits cockpit/runner",
			action: &Action{Capability: "fs.writeFile", Origin: "deploy/pin.memql:pinDigest"},
			want:   CockpitRunnerSurface,
		},
		{
			name:   "control capability inherits cockpit/runner regardless of origin",
			action: &Action{Capability: "integration.argocd.sync", Origin: "somewhere/else.memql:x"},
			want:   CockpitRunnerSurface,
		},
		{
			name:   "github control capability inherits cockpit/runner",
			action: &Action{Capability: "integration.github.tagRelease", Origin: "tools/gh.memql:tag"},
			want:   CockpitRunnerSurface,
		},
		{
			// An install action has no cluster to run inside: it places k3d on
			// the operator's laptop, edits /etc/hosts and installs a trust-store
			// CA, all before anything memQL exists to host it. The runner is not
			// a policy preference here -- it is the only placement that can
			// execute at all (#3371).
			name:   "install-pack origin inherits cockpit/runner",
			action: &Action{Capability: "shell.script", Origin: "install/actions.memql:installPinnedTool"},
			want:   CockpitRunnerSurface,
		},
		{
			name:   "ordinary action has no policy default",
			action: &Action{Capability: "shell.exec", Origin: "workbench/actions.memql:cloneRepo"},
			want:   "",
		},
		{
			name:   "nil action",
			action: nil,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PolicyDefaultSurface(tc.action); got != tc.want {
				t.Errorf("PolicyDefaultSurface = %q, want %q", got, tc.want)
			}
		})
	}
}
