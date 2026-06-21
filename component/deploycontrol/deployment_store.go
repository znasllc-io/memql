// deployment_store.go persists deployments as first-class
// v1:cluster:deployment records (#1872, foundation of epic #1871).
//
// Before this, a deploy left no trace in the data layer -- image
// digests lived only in releases/*.yaml on disk, and there was no
// deployment entity, status, or history to query. The console write
// RPCs (DeployStaging / Promote) now write a record at deploy start
// (status in_progress) and transition it as the rollout resolves
// (succeeded on a clean promote, failed on error). Each write appends a
// new payload version under the SAME concept id (the deploymentId), so
// the lifecycle is reconstructable asOf any time, and the CDC path emits
// graph.node.created/updated.<partition>.v1:cluster:deployment -- the
// same event stream the cockpit Deployments view subscribes to.
//
// Persistence is best-effort: a bookkeeping write must never block or
// fail a deploy. When no engine is wired (e.g. unit tests) every method
// is a no-op. Errors are logged at warn, never returned to the RPC.
package deploycontrol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

// deploymentEnvFor maps a console env ("staging"/"prod") onto the
// v1:cluster:deployment `environment` enum member.
func deploymentEnvFor(consoleEnv string) string {
	switch consoleEnv {
	case "prod":
		return "production"
	case "staging":
		return "staging"
	default:
		return "development"
	}
}

// deploymentProviderFor returns the deploy target backing a console
// env. Staging + prod both run on AKS today, so the provider is
// "azure"; MEMQL_DEPLOY_PROVIDER overrides for a non-default target.
func deploymentProviderFor(consoleEnv string) string {
	if p := strings.TrimSpace(os.Getenv("MEMQL_DEPLOY_PROVIDER")); p != "" {
		return p
	}
	return "azure"
}

// resolveImageDigest reads releases/<version>.yaml and returns a
// representative pinned image digest (the lexicographically-first
// component's digest, deterministic across calls). Returns "" when the
// lockfile is absent / unparseable -- digest is best-effort metadata,
// not a precondition for recording the deployment.
func (s *Service) resolveImageDigest(version string) string {
	if version == "" {
		return ""
	}
	lfPath := filepath.Join(s.repoRoot, "releases", version+".yaml")
	raw, err := os.ReadFile(lfPath)
	if err != nil {
		return ""
	}
	lf, err := ParseLockfile(raw)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(lf.Components))
	for name := range lf.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if d := lf.Components[name].Digest; d != "" {
			return d
		}
	}
	return ""
}

// recordDeployment writes a new v1:cluster:deployment row in the
// in_progress state and returns the generated deploymentId. Returns ""
// when no engine is wired or the write fails -- the caller treats ""
// as "nothing to transition" and skips the follow-up. The deploymentId
// doubles as the concept row id, so the later transition appends to the
// same time-series timeline.
func (s *Service) recordDeployment(ctx context.Context, consoleEnv, version, digest string, act actor) string {
	if s == nil || s.engine == nil {
		return ""
	}
	deploymentID := id.NewShortId()
	args := map[string]any{
		"deploymentId": deploymentID,
		"status":       "in_progress",
		"version":      version,
		"imageDigest":  digest,
		"provider":     deploymentProviderFor(consoleEnv),
		"environment":  deploymentEnvFor(consoleEnv),
		"region":       strings.TrimSpace(os.Getenv("MEMQL_REGION")),
		"clusterId":    strings.TrimSpace(os.Getenv("MEMQL_CLUSTER_ID")),
		"triggeredBy":  act.userID,
	}
	query := "mutationCreateDeployment(" + renderDeploymentArgs(args) + ")"
	if _, err := s.engine.Execute(ctx, query); err != nil {
		if s.logger != nil {
			s.logger.Warn("deploycontrol: persist deployment record failed (deploy continues)",
				"error", err, "env", consoleEnv, "version", version)
		}
		return ""
	}
	return deploymentID
}

// transitionDeployment appends a status transition for an existing
// deployment record. No-op when deploymentID is empty (no record was
// written) or no engine is wired.
func (s *Service) transitionDeployment(ctx context.Context, deploymentID, status string) {
	if s == nil || s.engine == nil || deploymentID == "" {
		return
	}
	args := map[string]any{
		"deploymentId": deploymentID,
		"status":       status,
	}
	query := "mutationUpdateDeploymentStatus(" + renderDeploymentArgs(args) + ")"
	if _, err := s.engine.Execute(ctx, query); err != nil && s.logger != nil {
		s.logger.Warn("deploycontrol: persist deployment transition failed",
			"error", err, "deployment_id", deploymentID, "status", status)
	}
}

// transitionStatusForErr maps a write-action's run error onto the
// terminal deployment status: nil -> succeeded, otherwise failed.
func transitionStatusForErr(runErr error) string {
	if runErr == nil {
		return "succeeded"
	}
	return "failed"
}

// renderDeploymentArgs turns a flat arg map into a MemQL object literal
// ({key: "val", ...}) with bare identifier keys and JSON-escaped
// values. Mirrors renderQueryArgs in component/grpc. Keys are emitted in
// sorted order so the rendered query is deterministic (stable logs +
// testability).
func renderDeploymentArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		vb, _ := json.Marshal(args[k])
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}
