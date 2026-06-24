// deploy.go implements the two target-agnostic deploy actions (#1878,
// epic #1871) on top of the deploy-driver abstraction (driver.go) and
// the persisted deployment records (#1872):
//
//   - Deploy(deploymentId): take a cut `pending` deployment record
//     (created by CutVersion, #1877), select the driver by the record's
//     provider, apply it, and transition the record
//     in_progress -> succeeded | failed.
//
//   - RollbackDeployment(toDeploymentId): redeploy a prior SUCCEEDED
//     deployment's stored image digest through the SAME driver. This is
//     NOT a blue-green color flip -- the previous color only survives the
//     scale-down window, so a rollback must re-pin (re-ship) the
//     historical digest. It creates a NEW deployment record pointing at
//     the historical digest (previousDeploymentId -> the target) and runs
//     it through the driver, landing in `rolled_back` on success.
//
// Deploy is developer-or-above gated (#1876): developer/admin/owner may
// ship a cut version forward. RollbackDeployment is gated more strictly
// -- owner-only (#1876) -- so developer and admin can deploy forward but
// not roll back. Both emit exactly one audit event via finishWrite.
package deploycontrol

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// deploymentRecord is the folded current state of a v1:cluster:deployment
// row, read from deploymentById's append-only timeline.
type deploymentRecord struct {
	deploymentID string
	status       string
	version      string
	imageDigest  string
	provider     string
	environment  string
	region       string
	clusterID    string
}

// Deploy ships a cut `pending` deployment record (#1877) to its target,
// selecting the driver by the record's provider (azure)
// and transitioning the record in_progress -> succeeded | failed (#1878).
// Owner/admin gated; emits exactly one audit event.
func (s *Service) Deploy(ctx context.Context, req *memqlv1.DeployRequest) (*memqlv1.ActionResult, error) {
	deploymentID := strings.TrimSpace(req.GetDeploymentId())
	if deploymentID == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: deployment_id is required")
	}
	detail := map[string]any{"deploymentId": deploymentID}
	// Forward deploy is developer-or-above (#1876): developer/admin/owner
	// may ship a cut version.
	act, err := s.authorizeDeploy(ctx, "deploy", detail)
	if err != nil {
		return nil, err
	}

	rec, err := s.loadDeployment(ctx, deploymentID)
	if err != nil {
		return s.finishWrite(ctx, "deploy", act, detail, "", err,
			map[string]string{"deploymentId": deploymentID}), nil
	}
	detail["version"] = rec.version
	detail["provider"] = rec.provider
	detail["environment"] = rec.environment

	driver, derr := selectDriver(s.exec, rec.provider)
	if derr != nil {
		// Unknown provider: record never leaves pending in a runnable
		// sense, so mark it failed and surface the reason.
		s.transitionDeployment(ctx, deploymentID, "failed")
		return s.finishWrite(ctx, "deploy", act, detail, "", derr,
			map[string]string{"deploymentId": deploymentID, "provider": rec.provider}), nil
	}

	// Automation-driven kick-off (#2115, MEMQL_DEPLOY_AUTOMATION_DRIVEN):
	// transition pending -> in_progress and STOP. The in_progress
	// transition emits the v1:cluster:deployment CDC update that the
	// deploy pack's driveDeploymentInProgress automation (E2.3) consumes;
	// that automation fires deployRunPromote (the SAME promote.sh effect)
	// and owns the succeeded|failed terminal transition. The RPC returns
	// an async ack immediately; callers poll GetDeploymentStatus / the
	// deployment concept for resolution.
	if s.automationDriven {
		s.transitionDeployment(ctx, deploymentID, "in_progress")
		return s.finishWrite(ctx, "deploy", act, detail, asyncDeployAck(deploymentID), nil, map[string]string{
			"deploymentId": deploymentID,
			"version":      rec.version,
			"provider":     rec.provider,
			"environment":  rec.environment,
			"status":       "in_progress",
			"async":        "true",
		}), nil
	}

	// Synchronous Go apply (authoritative until the owner-gated staging
	// cutover, #2115): pending -> in_progress, apply, -> succeeded | failed.
	s.transitionDeployment(ctx, deploymentID, "in_progress")
	out, runErr := driver.apply(ctx, s.specFor(deploymentID, rec))
	s.transitionDeployment(ctx, deploymentID, transitionStatusForErr(runErr))

	return s.finishWrite(ctx, "deploy", act, detail, out, runErr, map[string]string{
		"deploymentId": deploymentID,
		"version":      rec.version,
		"provider":     rec.provider,
		"environment":  rec.environment,
	}), nil
}

// RollbackDeployment redeploys a prior SUCCEEDED deployment's stored
// image digest through the SAME driver (#1878). It is NOT a blue-green
// color flip: it creates a NEW deployment record pointing at the
// historical digest (previousDeploymentId -> toDeploymentId) and runs
// that record through the driver, landing in `rolled_back` on success or
// `failed` on driver error. Rollback is owner-only (#1876); emits one
// audit event.
func (s *Service) RollbackDeployment(ctx context.Context, req *memqlv1.RollbackDeploymentRequest) (*memqlv1.ActionResult, error) {
	toID := strings.TrimSpace(req.GetToDeploymentId())
	if toID == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: to_deployment_id is required")
	}
	detail := map[string]any{"toDeploymentId": toID}
	// rollback is owner-only (#1876): not even admin may roll back.
	act, err := s.authorizeOwner(ctx, "rollback_deployment", detail)
	if err != nil {
		return nil, err
	}

	target, err := s.loadDeployment(ctx, toID)
	if err != nil {
		return s.finishWrite(ctx, "rollback_deployment", act, detail, "", err,
			map[string]string{"toDeploymentId": toID}), nil
	}
	// Only a SUCCEEDED historical deployment is a valid rollback target:
	// it is the last known-good digest for the env.
	if target.status != "succeeded" {
		return s.finishWrite(ctx, "rollback_deployment", act, detail, "",
			fmt.Errorf("deployment %s has status %q; rollback target must be succeeded", toID, target.status),
			map[string]string{"toDeploymentId": toID, "targetStatus": target.status}), nil
	}
	if target.version == "" && target.imageDigest == "" {
		return s.finishWrite(ctx, "rollback_deployment", act, detail, "",
			fmt.Errorf("deployment %s has no recorded version/image digest to redeploy", toID),
			map[string]string{"toDeploymentId": toID}), nil
	}

	driver, derr := selectDriver(s.exec, target.provider)
	if derr != nil {
		return s.finishWrite(ctx, "rollback_deployment", act, detail, "", derr,
			map[string]string{"toDeploymentId": toID, "provider": target.provider}), nil
	}

	detail["version"] = target.version
	detail["provider"] = target.provider
	detail["environment"] = target.environment

	// Create the new rollback record at in_progress (this IS a deploy),
	// carrying the historical digest + previousDeploymentId provenance.
	newID, createErr := s.createRollbackDeployment(ctx, toID, target, act)
	if createErr != nil {
		return s.finishWrite(ctx, "rollback_deployment", act, detail, "", createErr,
			map[string]string{"toDeploymentId": toID}), nil
	}

	// Automation-driven kick-off (#2115): the rollback record was just
	// created at in_progress, which emits the CDC update the deploy pack's
	// driveDeploymentInProgress automation consumes -- it re-pins the
	// historical digest via deployRunPromote and owns the terminal
	// transition. Return an async ack immediately.
	//
	// PARITY NOTE (#2115): the automation drives in_progress -> succeeded
	// (it has no rollback-aware terminal), so an automation-driven
	// rollback lands in `succeeded` carrying previousDeploymentId
	// provenance, whereas the synchronous Go path lands it in
	// `rolled_back`. Reconciling the terminal status (teach the automation
	// a rollback edge, or accept succeeded+provenance) is part of the
	// owner-gated staging validation + Go-apply retirement (steps 5-6).
	if s.automationDriven {
		return s.finishWrite(ctx, "rollback_deployment", act, detail, asyncRollbackAck(toID, newID), nil, map[string]string{
			"toDeploymentId":  toID,
			"newDeploymentId": newID,
			"version":         target.version,
			"imageDigest":     target.imageDigest,
			"provider":        target.provider,
			"environment":     target.environment,
			"status":          "in_progress",
			"async":           "true",
		}), nil
	}

	out, runErr := driver.apply(ctx, s.specFor(newID, target))
	if runErr != nil {
		s.transitionDeployment(ctx, newID, "failed")
	} else {
		s.transitionDeployment(ctx, newID, "rolled_back")
	}

	return s.finishWrite(ctx, "rollback_deployment", act, detail, out, runErr, map[string]string{
		"toDeploymentId":  toID,
		"newDeploymentId": newID,
		"version":         target.version,
		"imageDigest":     target.imageDigest,
		"provider":        target.provider,
		"environment":     target.environment,
	}), nil
}

// asyncDeployAck is the message returned by the automation-driven Deploy
// kick-off (#2115): the RPC has transitioned the record to in_progress
// and the deploy pack automation owns promote + the terminal status.
// The async contract is: ok=true means "accepted + kicked off", NOT
// "deploy succeeded"; the caller polls the deployment concept (or
// GetDeploymentStatus) for the terminal status.
func asyncDeployAck(deploymentID string) string {
	return fmt.Sprintf("deploy kicked off (automation-driven): deployment %s -> in_progress; "+
		"the deploy pack drives promote + the terminal status. "+
		"Poll the deployment concept / GetDeploymentStatus for resolution.", deploymentID)
}

// asyncRollbackAck mirrors asyncDeployAck for the rollback record path:
// the new in_progress rollback record is created and the automation owns
// promote + terminal. ok=true means "accepted + kicked off".
func asyncRollbackAck(toID, newID string) string {
	return fmt.Sprintf("rollback kicked off (automation-driven): new deployment %s "+
		"(rollback to %s) -> in_progress; the deploy pack drives promote + the terminal status. "+
		"Poll the deployment concept / GetDeploymentStatus for resolution.", newID, toID)
}

// specFor builds the driver spec for applying a record under a given
// (possibly newly-minted, for rollback) deploymentId.
func (s *Service) specFor(deploymentID string, rec deploymentRecord) deploymentSpec {
	return deploymentSpec{
		deploymentID: deploymentID,
		version:      rec.version,
		imageDigest:  rec.imageDigest,
		consoleEnv:   consoleEnvFor(rec.environment),
		environment:  rec.environment,
		provider:     rec.provider,
	}
}

// loadDeployment reads a deployment's current state via deploymentById.
// The query returns the full append-only timeline oldest-to-newest; a
// status-only transition (updateDeploymentStatus) is a read-merge
// update that re-stamps every field, so the latest row carries the
// complete current state. We fold across the timeline (last non-empty
// wins per field, last row's status wins) to be robust regardless.
func (s *Service) loadDeployment(ctx context.Context, deploymentID string) (deploymentRecord, error) {
	if s == nil || s.engine == nil {
		return deploymentRecord{}, fmt.Errorf("no engine wired: cannot read deployment %s", deploymentID)
	}
	query := "deploymentById(" + renderDeploymentArgs(map[string]any{"deploymentId": deploymentID}) + ")"
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return deploymentRecord{}, fmt.Errorf("read deployment %s: %w", deploymentID, err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return deploymentRecord{}, fmt.Errorf("deployment %s not found", deploymentID)
	}

	rec := deploymentRecord{deploymentID: deploymentID}
	setIf := func(dst *string, v string) {
		if t := strings.TrimSpace(v); t != "" {
			*dst = t
		}
	}
	for _, node := range res.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		f := node.Payload.GetFields()
		// status: last row wins (track transitions), even when empty-guarded
		// by setIf -- status is always populated by the mutations.
		setIf(&rec.status, f["status"].GetStringValue())
		setIf(&rec.version, f["version"].GetStringValue())
		setIf(&rec.imageDigest, f["imageDigest"].GetStringValue())
		setIf(&rec.provider, f["provider"].GetStringValue())
		setIf(&rec.environment, f["environment"].GetStringValue())
		setIf(&rec.region, f["region"].GetStringValue())
		setIf(&rec.clusterID, f["clusterId"].GetStringValue())
	}
	return rec, nil
}

// createRollbackDeployment writes the new v1:cluster:deployment row for a
// rollback: in_progress, carrying the target's digest/version/provider/
// environment, previousDeploymentId pointing at the restored deployment,
// and an operator note. Returns the new deploymentId. A missing engine or
// failed write is a real error (the rollback cannot proceed without a
// record to drive).
func (s *Service) createRollbackDeployment(ctx context.Context, toID string, target deploymentRecord, act actor) (string, error) {
	if s == nil || s.engine == nil {
		return "", fmt.Errorf("no engine wired: cannot persist rollback record")
	}
	newID := id.NewShortId()
	note := fmt.Sprintf("rollback to deployment %s (version %s, digest %s)", toID, target.version, target.imageDigest)
	args := map[string]any{
		"deploymentId":         newID,
		"status":               "in_progress",
		"version":              target.version,
		"imageDigest":          target.imageDigest,
		"provider":             target.provider,
		"environment":          target.environment,
		"region":               target.region,
		"clusterId":            target.clusterID,
		"triggeredBy":          act.userID,
		"previousDeploymentId": toID,
		"notes":                note,
	}
	query := "createDeployment(" + renderDeploymentArgs(args) + ")"
	if _, err := s.engine.Execute(ctx, query); err != nil {
		return "", fmt.Errorf("create rollback deployment record: %w", err)
	}
	return newID, nil
}
