package actions

import (
	"strings"

	"github.com/znasllc-io/memql/component/harness/surfaceresolver"
)

// Execution-surface registry (memql#2220, action-library §7).
//
// An action is portable; WHERE it runs is not part of its identity (§7.1).
// This file is the engine's built-in surface registry plus the policy-default
// tier of the capability->surface resolver (§7.2). The pure precedence
// machinery lives in component/harness/surfaceresolver; here we (a) declare the
// concrete surfaces the engine ships with and (b) decide the policy default for
// an authored action so deploy actions resolve OUTSIDE the target cluster.
//
// Carrier/operator surfaces (workbench, computer-use:<machineId>, mcp:<server>)
// are NOT hardcoded here -- they register as v1:actions:surface rows and are
// supplied to the resolver alongside these builtins. Capability coverage is
// DATA (a surface lists the verbs it serves), so new surfaces need no engine
// change.

// CockpitRunnerSurface is the control-plane surface that runs an action's
// capability OUTSIDE the target cluster -- on the operator/CI cockpit runner,
// not inside the cluster being deployed (DEVOPS_DSL_BUNDLE_HANDOFF ADR,
// Execution model). Deploy actions (I8, #2222) resolve here via the policy
// default so e.g. integration.argocd.sync / shell.exec / fs.writeFile never run
// in-cluster.
const CockpitRunnerSurface = "cockpit/runner"

// cockpitRunnerCapabilities are the capability namespaces the cockpit/runner
// surface serves (action-library §7 surface list). A trailing ".*" is a
// namespace wildcard matched by the resolver (shell.* covers shell.exec, ...).
var cockpitRunnerCapabilities = []string{
	"shell.*",
	"fs.*",
	"http.*",
	"integration.argocd.*",
	"integration.github.*",
}

// BuiltinSurfaces returns the engine's built-in execution-surface registry as
// surfaceresolver.Surface projections. Today this is the single cockpit/runner
// control surface; carrier-supplied surfaces (workbench, computer-use, mcp) are
// merged in by the caller from v1:actions:surface rows. It is pure + allocates
// fresh slices so a caller may safely append registered surfaces.
func BuiltinSurfaces() []surfaceresolver.Surface {
	return []surfaceresolver.Surface{
		{
			ID:           CockpitRunnerSurface,
			Slug:         CockpitRunnerSurface,
			Kind:         "cockpit-runner",
			Capabilities: append([]string(nil), cockpitRunnerCapabilities...),
			Available:    true,
			// Low priority so the control surface wins availability failover
			// among the engine builtins; carrier surfaces set their own.
			Priority: 50,
		},
	}
}

// deployPackPrefixes are the DSL-tree path prefixes whose authored actions
// default (policy tier) to the cockpit/runner surface. I8 lands the deployment
// pack's actions under one of these and they inherit the cockpit/runner default
// with NO per-action annotation. (Origin is "<path>:<name>".)
var deployPackPrefixes = []string{
	"deployment/",
	"deploy/",
	"actions/deploy/",
}

// deployControlCapabilityPrefixes are capability namespaces that are control-
// plane by nature: they orchestrate a target cluster from the outside, so they
// default to cockpit/runner regardless of where the action is authored. This
// is the second inheritance path for I8 (a deploy action authored elsewhere
// still routes correctly).
var deployControlCapabilityPrefixes = []string{
	"integration.argocd.",
	"integration.github.",
}

// PolicyDefaultSurface returns the policy-tier default surface slug for an
// action (the middle tier of the §7.2 precedence: explicit > POLICY >
// availability). It is "" when no policy applies -- the resolver then falls
// through to availability failover.
//
// Deployment-pack actions (by DSL-tree origin) and control-plane capabilities
// (argocd/github orchestration) default to the cockpit/runner surface so they
// run outside the target cluster.
func PolicyDefaultSurface(a *Action) string {
	if a == nil {
		return ""
	}
	if isDeployPackOrigin(a.Origin) || hasDeployControlCapability(a.Capability) {
		return CockpitRunnerSurface
	}
	return ""
}

// isDeployPackOrigin reports whether an action's origin ("<path>:<name>") is
// under one of the deployment-pack DSL-tree prefixes.
func isDeployPackOrigin(origin string) bool {
	path := origin
	if i := strings.LastIndexByte(origin, ':'); i >= 0 {
		path = origin[:i]
	}
	for _, p := range deployPackPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// hasDeployControlCapability reports whether a capability is a control-plane
// verb that must run on the cockpit runner (outside the target cluster).
func hasDeployControlCapability(capability string) bool {
	for _, p := range deployControlCapabilityPrefixes {
		if strings.HasPrefix(capability, p) {
			return true
		}
	}
	return false
}
