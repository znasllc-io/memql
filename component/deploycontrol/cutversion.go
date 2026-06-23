// cutversion.go implements the "cut a version" flow (#1877, epic
// #1871). With deployments now persisted (#1872), the current version is
// known, so memQL can propose the next semver and let an owner cut it.
//
// SuggestNextVersion (read) reads the latest succeeded deployment's
// version for an environment and proposes the next major / minor / patch.
// CutVersion (write) creates a new pending v1:cluster:deployment record
// at the chosen version with the resolved image digest -- ready for the
// deploy driver (#1878) to ship. Both are developer-or-above gated
// (#1876): cutting a version is a forward-deploy action, so
// developer/admin/owner may do it (SuggestNextVersion is its read
// companion and shares the gate so a developer can size the cut). The
// DSL mirror is spec("requiresDeveloperOrAbove") in
// dsl/deployment/specs.memql.
package deploycontrol

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// SuggestNextVersion proposes the next major/minor/patch off the env's
// current version (#1877). Read-only; developer-or-above gated as the
// read companion to CutVersion (#1876), not audited on success (a
// denial emits a blocked audit event via authorizeDeploy).
func (s *Service) SuggestNextVersion(ctx context.Context, req *memqlv1.SuggestNextVersionRequest) (*memqlv1.SuggestNextVersionResult, error) {
	consoleEnv := req.GetEnv()
	if !validEnvs[consoleEnv] {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid env %q (want staging|prod)", consoleEnv)
	}
	if _, err := s.authorizeDeploy(ctx, "suggest_version", map[string]any{"env": consoleEnv}); err != nil {
		return nil, err
	}

	current, _, source := s.currentVersionForEnv(ctx, consoleEnv)

	// Base the proposals on the current version, or on 0.0.0 when no
	// current version is known (so the suggestions are still useful for a
	// first-ever cut: 1.0.0 / 0.1.0 / 0.0.1).
	base := semver{}
	if current != "" {
		v, err := parseSemver(current)
		if err != nil {
			// A malformed stored version shouldn't 500 a read; surface the
			// raw current value with no proposals rather than fail.
			return &memqlv1.SuggestNextVersionResult{CurrentVersion: current, Source: source}, nil
		}
		base = v
	}
	major, _ := base.bump("major")
	minor, _ := base.bump("minor")
	patch, _ := base.bump("patch")
	return &memqlv1.SuggestNextVersionResult{
		CurrentVersion: current,
		NextMajor:      major.String(),
		NextMinor:      minor.String(),
		NextPatch:      patch.String(),
		Source:         source,
	}, nil
}

// CutVersion creates a new pending deployment at the chosen next version
// (#1877). The target is the current version bumped by `bump` (patch
// default), or an explicit `version` override. Developer-or-above gated
// (#1876); emits exactly one audit event (success / failure). Invalid or
// duplicate versions are rejected.
func (s *Service) CutVersion(ctx context.Context, req *memqlv1.CutVersionRequest) (*memqlv1.ActionResult, error) {
	consoleEnv := req.GetEnv()
	if !validEnvs[consoleEnv] {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid env %q (want staging|prod)", consoleEnv)
	}
	bump := strings.TrimSpace(req.GetBump())
	explicit := strings.TrimSpace(req.GetVersion())

	// Argument-shape validation BEFORE the auth gate (mirrors the other
	// write RPCs, which reject empty/invalid args before authorize). An
	// explicit version must be clean semver; otherwise the bump part must
	// be valid. Both are caller errors -> InvalidArgument, not audited.
	if explicit != "" {
		if _, err := parseSemver(explicit); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "deploy console: %v", err)
		}
	} else if _, err := (semver{}).bump(bump); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: %v", err)
	}

	detail := map[string]any{"env": consoleEnv, "bump": bump, "requestedVersion": explicit}
	// Cutting a version is a forward-deploy action: developer-or-above
	// (#1876).
	act, err := s.authorizeDeploy(ctx, "cut_version", detail)
	if err != nil {
		return nil, err
	}

	// Resolve the target version + the set of versions already taken for
	// this env (the duplicate guard).
	current, taken, source := s.currentVersionForEnv(ctx, consoleEnv)
	var target string
	if explicit != "" {
		v, _ := parseSemver(explicit) // already validated above
		target = v.String()
	} else {
		base := semver{}
		if current != "" {
			v, perr := parseSemver(current)
			if perr != nil {
				// The current version can't be parsed but a cut was requested
				// by bump -- record a failed action rather than guess.
				detail["currentVersion"] = current
				return s.finishWrite(ctx, "cut_version", act, detail, "",
					fmt.Errorf("current version %q is not clean semver; pass an explicit version", current),
					map[string]string{"env": consoleEnv}), nil
			}
			base = v
		}
		next, _ := base.bump(bump) // bump already validated above
		target = next.String()
	}

	detail["version"] = target
	detail["currentVersion"] = current

	// Duplicate guard: reject a version already cut for this env (any
	// status). Cross-env reuse (cut for staging, later promote to prod) is
	// fine and intentionally not blocked.
	if taken[target] {
		return s.finishWrite(ctx, "cut_version", act, detail, "",
			fmt.Errorf("version %q already exists for %s", target, consoleEnv),
			map[string]string{"env": consoleEnv, "version": target}), nil
	}

	// Resolve the image digest (best-effort: empty when the release
	// lockfile for the new version isn't assembled yet) and create the
	// pending record.
	digest := s.resolveImageDigest(target)
	deploymentID, createErr := s.createPendingDeployment(ctx, consoleEnv, target, digest, act)

	bumpLabel := bump
	if explicit == "" && bumpLabel == "" {
		bumpLabel = "patch"
	}
	if explicit != "" {
		bumpLabel = "explicit"
	}
	resultDetails := map[string]string{
		"env":            consoleEnv,
		"version":        target,
		"bump":           bumpLabel,
		"currentVersion": current,
		"source":         source,
		"deploymentId":   deploymentID,
		"imageDigest":    digest,
	}
	output := fmt.Sprintf("cut %s for %s (deployment %s)", target, consoleEnv, deploymentID)
	return s.finishWrite(ctx, "cut_version", act, detail, output, createErr, resultDetails), nil
}

// currentVersionForEnv resolves the version to bump from for an env and
// the set of versions already taken (for the duplicate guard). It prefers
// the highest-semver SUCCEEDED deployment record for the env; failing
// that, it falls back to the on-disk overlay's promoted version. taken
// collects EVERY stored version for the env (any status) so an in-flight
// pending/in_progress cut still blocks a re-cut. source is one of
// "deployment" / "overlay" / "none".
func (s *Service) currentVersionForEnv(ctx context.Context, consoleEnv string) (current string, taken map[string]bool, source string) {
	taken = map[string]bool{}
	targetEnv := deploymentEnvFor(consoleEnv)

	var best semver
	haveBest := false
	if s.engine != nil {
		clusterID := strings.TrimSpace(os.Getenv("MEMQL_CLUSTER_ID"))
		query := "deploymentsForCluster(" + renderDeploymentArgs(map[string]any{"clusterId": clusterID}) + ")"
		res, err := s.engine.Execute(ctx, query)
		switch {
		case err != nil:
			if s.logger != nil {
				s.logger.Warn("deploycontrol: query deployments for cut-version failed (falling back to overlay)",
					"error", err, "env", consoleEnv)
			}
		case res != nil && res.Bundle != nil:
			for _, node := range res.Bundle.Nodes {
				if node == nil || node.Payload == nil {
					continue
				}
				fields := node.Payload.GetFields()
				if strings.TrimSpace(fields["environment"].GetStringValue()) != targetEnv {
					continue
				}
				ver := strings.TrimSpace(fields["version"].GetStringValue())
				if ver == "" {
					continue
				}
				taken[ver] = true
				if fields["status"].GetStringValue() != "succeeded" {
					continue
				}
				if v, perr := parseSemver(ver); perr == nil {
					if !haveBest || v.compare(best) > 0 {
						best = v
						haveBest = true
					}
				}
			}
		}
	}
	if haveBest {
		return best.String(), taken, "deployment"
	}
	if v := s.overlayPromotedVersion(consoleEnv); v != "" {
		return v, taken, "overlay"
	}
	return "", taken, "none"
}

// overlayPromotedVersion reads the on-disk kustomization overlay for an
// env and returns its promoted version (empty when absent / unparseable
// / unpromoted, e.g. the staging overlay carries no promotion comment).
func (s *Service) overlayPromotedVersion(consoleEnv string) string {
	overlayPath := filepath.Join(s.repoRoot, "deploy", "k8s", "overlays", consoleEnv, "kustomization.yaml")
	raw, err := os.ReadFile(overlayPath)
	if err != nil {
		return ""
	}
	overlay, err := ParseOverlay(raw)
	if err != nil {
		return ""
	}
	return overlay.PromotedVersion
}

// createPendingDeployment writes a new v1:cluster:deployment row in the
// pending state and returns its deploymentId. Unlike recordDeployment
// (the best-effort bookkeeping write on the deploy path), this is the
// PRIMARY action of CutVersion, so a missing engine or a failed write is
// a real error the caller surfaces as ok=false.
func (s *Service) createPendingDeployment(ctx context.Context, consoleEnv, version, digest string, act actor) (string, error) {
	if s == nil || s.engine == nil {
		return "", fmt.Errorf("no engine wired: cannot persist deployment record")
	}
	deploymentID := id.NewShortId()
	args := map[string]any{
		"deploymentId": deploymentID,
		"status":       "pending",
		"version":      version,
		"imageDigest":  digest,
		"provider":     deploymentProviderFor(consoleEnv),
		"environment":  deploymentEnvFor(consoleEnv),
		"region":       strings.TrimSpace(os.Getenv("MEMQL_REGION")),
		"clusterId":    strings.TrimSpace(os.Getenv("MEMQL_CLUSTER_ID")),
		"triggeredBy":  act.userID,
	}
	query := "createDeployment(" + renderDeploymentArgs(args) + ")"
	if _, err := s.engine.Execute(ctx, query); err != nil {
		return "", err
	}
	return deploymentID, nil
}
