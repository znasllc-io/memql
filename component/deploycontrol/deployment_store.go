// deployment_store.go persists deployments as first-class
// v1:cluster:deployment records (#1872, foundation of epic #1871).
//
// Before this, a deploy left no trace in the data layer -- image
// digests lived only in releases/*.yaml on disk, and there was no
// deployment entity, status, or history to query. The console write
// RPCs now write a record at deploy start (status in_progress) and transition
// it as the rollout resolves (succeeded on a clean run, failed on error). Each
// write appends a
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
)

// deploymentProvider returns the deploy target this installation ships to.
// The cloud target runs on AKS, so the provider is "azure";
// MEMQL_DEPLOY_PROVIDER overrides for a non-default target.
//
// This is the axis that SURVIVED epic memql#3943: local-vs-cloud is a deploy
// TARGET, which the parity standard keeps, and it was always separate from the
// environment concept the epic removed.
func deploymentProvider() string {
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
	query := "updateDeploymentStatus(" + renderDeploymentArgs(args) + ")"
	if _, err := s.engine.Execute(ctx, query); err != nil && s.logger != nil {
		s.logger.Warn("deploycontrol: persist deployment transition failed",
			"error", err, "deployment_id", deploymentID, "status", status)
	}
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
