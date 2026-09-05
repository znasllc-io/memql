package deploycontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// rollout_restart.go adds the one verb this substrate lacked: restarting a
// Deployment from inside the cluster (epic memql#4794).
//
// ===========================================================================
// WHY IT IS AN API CALL AND NOT A CAPABILITY SCRIPT
// ===========================================================================
// The obvious implementation is `kubectl rollout restart deploy/<name>`, and
// it cannot work where this runs. The engine's runtime image is DISTROLESS:
// no shell, no kubectl, and `.dockerignore` keeps the scripts/ tree out of the
// build context entirely. A capability script here passes every local test and
// then never runs on a single deployed cluster -- which is exactly the shape
// the DNS work found the same week, and the reason this file exists rather
// than a script.
//
// What kubectl actually does for `rollout restart` is one strategic-merge
// PATCH stamping a timestamp annotation onto the pod TEMPLATE. Changing the
// template is what makes the Deployment controller roll: old pods keep serving
// until the new ones are ready, which is the property the package roll depends
// on (design section H -- "old pods serve until new pods are healthy").
//
// So this is not an approximation of the plugin verb the way RunRolloutAction
// refuses to be. `rollout restart` has a single, stable, documented API form,
// and this is it.

// restartedAtAnnotation is the annotation kubectl itself stamps. Spelled the
// same on purpose: an operator running `kubectl rollout restart` afterwards
// overwrites this one rather than adding a second, so the two paths cannot
// leave a Deployment carrying a confusing pair.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// saNamespacePath is where a pod learns its own namespace.
const saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace" // #nosec G101 -- a path, not a credential

// NamespaceFromEnv reports the namespace this process is running in.
//
// The projected file first and the environment second, because the file is
// what the API server itself considers authoritative and MEMQL_NAMESPACE is a
// convenience an operator can set wrongly. Empty when neither is present,
// which every caller here treats as "not in a cluster".
func NamespaceFromEnv() string {
	if raw, err := os.ReadFile(saNamespacePath); err == nil {
		if ns := strings.TrimSpace(string(raw)); ns != "" {
			return ns
		}
	}
	return strings.TrimSpace(os.Getenv("MEMQL_NAMESPACE"))
}

// RestartDeployment performs the in-cluster equivalent of
// `kubectl rollout restart deployment/<name>` in namespace.
//
// It is idempotent in the only sense that matters: calling it twice stamps two
// different timestamps and therefore rolls twice, which is what "restart"
// means. It is NOT a no-op on a second call, and a caller that wants one has
// to decide that for itself -- the package pipeline does, by rolling only when
// the active-set pointer actually moved.
//
// Refuses with ReasonNoClusterAPI when this process is not in a cluster, so a
// developer machine gets a named refusal rather than a confusing dial error.
func RestartDeployment(ctx context.Context, namespace, name string) error {
	ns := strings.TrimSpace(namespace)
	dep := strings.TrimSpace(name)
	if ns == "" || dep == "" {
		return fmt.Errorf("deploycontrol: namespace and deployment name are required")
	}
	if !InClusterAvailable() {
		return fmt.Errorf("%s: this process is not running inside a cluster, so it cannot restart %s/%s",
			ReasonNoClusterAPI, ns, dep)
	}

	exec, err := newInClusterExecutor("")
	if err != nil {
		return err
	}

	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						restartedAtAnnotation: time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	})
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, dep)
	if _, err := exec.do(ctx, "PATCH", path, "application/strategic-merge-patch+json", patch); err != nil {
		// A 403 HERE HAS ONE REPAIR, AND IT IS NOT IN THIS REPOSITORY'S CODE
		// (memql#4933). The engine runs as `memql-engine`, which by design
		// holds no Kubernetes API privilege, so a fresh instance reaches this
		// call with no permission to make it. The API server's own sentence
		// names the account and the verb and is kept; what it cannot know is
		// that the fix is a Role, that the Role must name this Deployment in
		// `resourceNames`, and that the list has to agree with a value set
		// somewhere else entirely.
		var status *StatusError
		if errors.As(err, &status) && status.Code == http.StatusForbidden {
			return fmt.Errorf("restarting %s/%s: %w\n\nThe node's ServiceAccount may not patch this "+
				"Deployment. A DSL roll needs a Role granting apps/deployments `patch`, with %q in its "+
				"resourceNames, bound to the account this pod runs as -- "+
				"deploy/k8s/components/packages-roll-rbac ships one. Its resourceNames and "+
				"%s must name the same workloads: a name in one and not the other is either this 403 "+
				"or a permission nothing uses", ns, dep, err, dep, rollTargetsEnvName)
		}
		return fmt.Errorf("restarting %s/%s: %w", ns, dep, err)
	}
	return nil
}

// rollTargetsEnvName is named here rather than imported: component/packages
// owns the variable and depends on this package, so reading the constant back
// would close a cycle. The two are held together by
// TestRollTargetsEnvNameMatchesTheOwner, which lives on the packages side
// because that is the only side that can see both.
const rollTargetsEnvName = "MEMQL_PACKAGES_ROLL_TARGETS"

// RollTargetsEnvNameForHint exposes that literal to the test that pins it.
// Exported for one caller and named so that reading it as a general accessor
// is uncomfortable -- the value an operator should read is
// packages.RollTargetsEnv.
func RollTargetsEnvNameForHint() string { return rollTargetsEnvName }
