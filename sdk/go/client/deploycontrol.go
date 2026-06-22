package client

// This file is the hand-rolled SDK surface for the DeployControlService
// (znasllc-io/memql#725 + #728) -- the gRPC service behind the memQL
// Deployment Console / external cockpit (issues #144/#145).
//
// Carve-out rationale (per sdk/go/CLAUDE.md): DeployControlService is a
// secondary UNARY service mounted on the same listener as MemqlService,
// not a DSL query/mutation, so it has no generated named primitive. It
// lives on the SDK side of the wire (the consumer stays uniform) and
// translates the proto types into plain Go structs here so cockpit code
// never imports memqlv1.

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// DeployControlClient is a thin typed client over DeployControlService.
// Construct it from an established Connection via NewDeployControlClient.
type DeployControlClient struct {
	grpc memqlv1.DeployControlServiceClient
}

// NewDeployControlClient builds a DeployControlClient over an existing
// Connection's gRPC channel.
func NewDeployControlClient(conn *Connection) *DeployControlClient {
	return &DeployControlClient{
		grpc: memqlv1.NewDeployControlServiceClient(conn.ClientConn()),
	}
}

// -----------------------------------------------------------------------------
// Plain Go result types (no proto leakage into consumers)
// -----------------------------------------------------------------------------

// ComponentDigest is one deployed component's pinned image.
type ComponentDigest struct {
	Name   string
	Digest string
	Repo   string
}

// ArgoStatus is the Argo CD application status.
type ArgoStatus struct {
	SyncStatus       string
	HealthStatus     string
	LastSyncRevision string
	LastSyncAt       string
	OutOfSync        bool
}

// RolloutStatus is one Argo Rollout's progressive-delivery state.
type RolloutStatus struct {
	Name                 string
	Kind                 string // "bluegreen" | "canary"
	Phase                string
	ActiveColor          string
	PreviewColor         string
	CanaryWeight         int32
	CurrentStep          int32
	LatestAnalysisResult string
}

// GateLeg is one leg of the deploy gate.
type GateLeg struct {
	Name   string
	Passed bool
	Detail string
}

// GateResult is the latest deploy-gate outcome.
type GateResult struct {
	Result string // "pass" | "fail" | "unknown"
	Legs   []GateLeg
	RanAt  string
}

// DeploymentStatus is the aggregate per-env deployment view.
type DeploymentStatus struct {
	Env           string
	Version       string
	EngineVersion string
	ValidatedAt   string
	Gate          string
	Components    []ComponentDigest
	Argocd        ArgoStatus
	Rollouts      []RolloutStatus
	GateResult    GateResult
}

// ActionResult is the uniform return for every write action.
type ActionResult struct {
	OK            bool
	Message       string
	AuditEventID  string
	CorrelationID string
	Details       map[string]string
}

// NextVersionSuggestion carries the current version plus the three
// next-version proposals from SuggestNextVersion (#1877).
type NextVersionSuggestion struct {
	CurrentVersion string
	NextMajor      string
	NextMinor      string
	NextPatch      string
	Source         string // "deployment" | "overlay" | "none"
}

// -----------------------------------------------------------------------------
// Methods (mirror the 5 RPCs)
// -----------------------------------------------------------------------------

// GetDeploymentStatus returns the full deployment picture for env
// ("staging" | "prod").
func (c *DeployControlClient) GetDeploymentStatus(ctx context.Context, env string) (DeploymentStatus, error) {
	resp, err := c.grpc.GetDeploymentStatus(ctx, &memqlv1.GetDeploymentStatusRequest{Env: env})
	if err != nil {
		return DeploymentStatus{}, fmt.Errorf("deploy console: get status %s: %w", env, err)
	}
	return deploymentStatusFromProto(resp), nil
}

// DeployStaging deploys the given release version into staging.
func (c *DeployControlClient) DeployStaging(ctx context.Context, version string) (ActionResult, error) {
	resp, err := c.grpc.DeployStaging(ctx, &memqlv1.DeployStagingRequest{Version: version})
	return actionResultFromProto(resp), wrapActionErr("deploy_staging", err)
}

// Promote copies a validated staging release into prod.
func (c *DeployControlClient) Promote(ctx context.Context, version string) (ActionResult, error) {
	resp, err := c.grpc.Promote(ctx, &memqlv1.PromoteRequest{Version: version})
	return actionResultFromProto(resp), wrapActionErr("promote", err)
}

// Rollback reverts the overlay commit identified by commitSha for env.
func (c *DeployControlClient) Rollback(ctx context.Context, env, commitSha string) (ActionResult, error) {
	resp, err := c.grpc.Rollback(ctx, &memqlv1.RollbackRequest{Env: env, CommitSha: commitSha})
	return actionResultFromProto(resp), wrapActionErr("rollback", err)
}

// RolloutAction promotes or aborts an Argo Rollout (action ∈
// {"promote","abort"}).
func (c *DeployControlClient) RolloutAction(ctx context.Context, env, rollout, action string) (ActionResult, error) {
	resp, err := c.grpc.RolloutAction(ctx, &memqlv1.RolloutActionRequest{Env: env, Rollout: rollout, Action: action})
	return actionResultFromProto(resp), wrapActionErr("rollout_action", err)
}

// SuggestNextVersion reads the latest succeeded deployment's version for
// env ("staging" | "prod") and proposes the next major / minor / patch
// semver (#1877). Read-only; developer/admin/owner gated server-side.
func (c *DeployControlClient) SuggestNextVersion(ctx context.Context, env string) (NextVersionSuggestion, error) {
	resp, err := c.grpc.SuggestNextVersion(ctx, &memqlv1.SuggestNextVersionRequest{Env: env})
	if err != nil {
		return NextVersionSuggestion{}, fmt.Errorf("deploy console: suggest next version %s: %w", env, err)
	}
	return nextVersionSuggestionFromProto(resp), nil
}

// CutVersion creates a new pending v1:cluster:deployment record (#1877)
// at the chosen next version for env, with the resolved image digest --
// ready for Deploy to ship. bump ∈ {"major","minor","patch"} selects the
// semver part to increment off the current version (empty defaults to
// patch); version is an optional explicit override (when set, bump is
// ignored). Developer/admin/owner gated server-side.
func (c *DeployControlClient) CutVersion(ctx context.Context, env, bump, version string) (ActionResult, error) {
	resp, err := c.grpc.CutVersion(ctx, &memqlv1.CutVersionRequest{Env: env, Bump: bump, Version: version})
	return actionResultFromProto(resp), wrapActionErr("cut_version", err)
}

// Deploy ships a cut (pending) v1:cluster:deployment record to its target
// (#1878), selecting the driver by the record's stored provider
// (docker-local | azure) and transitioning the record in_progress ->
// succeeded | failed. Developer/admin/owner gated server-side.
func (c *DeployControlClient) Deploy(ctx context.Context, deploymentId string) (ActionResult, error) {
	resp, err := c.grpc.Deploy(ctx, &memqlv1.DeployRequest{DeploymentId: deploymentId})
	return actionResultFromProto(resp), wrapActionErr("deploy", err)
}

// RollbackDeployment redeploys a prior SUCCEEDED deployment's stored image
// digest through the same driver (#1878) -- NOT a blue-green color flip. A
// new deployment record is created pointing at the historical digest and
// run through the driver, landing in rolled_back on success. Owner-only
// server-side (#1876).
func (c *DeployControlClient) RollbackDeployment(ctx context.Context, toDeploymentId string) (ActionResult, error) {
	resp, err := c.grpc.RollbackDeployment(ctx, &memqlv1.RollbackDeploymentRequest{ToDeploymentId: toDeploymentId})
	return actionResultFromProto(resp), wrapActionErr("rollback_deployment", err)
}

// -----------------------------------------------------------------------------
// proto -> SDK translation
// -----------------------------------------------------------------------------

func wrapActionErr(verb string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("deploy console: %s: %w", verb, err)
}

func deploymentStatusFromProto(p *memqlv1.DeploymentStatus) DeploymentStatus {
	if p == nil {
		return DeploymentStatus{}
	}
	out := DeploymentStatus{
		Env:           p.GetEnv(),
		Version:       p.GetVersion(),
		EngineVersion: p.GetEngineVersion(),
		ValidatedAt:   p.GetValidatedAt(),
		Gate:          p.GetGate(),
	}
	for _, comp := range p.GetComponents() {
		out.Components = append(out.Components, ComponentDigest{
			Name:   comp.GetName(),
			Digest: comp.GetDigest(),
			Repo:   comp.GetRepo(),
		})
	}
	if a := p.GetArgocd(); a != nil {
		out.Argocd = ArgoStatus{
			SyncStatus:       a.GetSyncStatus(),
			HealthStatus:     a.GetHealthStatus(),
			LastSyncRevision: a.GetLastSyncRevision(),
			LastSyncAt:       a.GetLastSyncAt(),
			OutOfSync:        a.GetOutOfSync(),
		}
	}
	for _, r := range p.GetRollouts() {
		out.Rollouts = append(out.Rollouts, RolloutStatus{
			Name:                 r.GetName(),
			Kind:                 r.GetKind(),
			Phase:                r.GetPhase(),
			ActiveColor:          r.GetActiveColor(),
			PreviewColor:         r.GetPreviewColor(),
			CanaryWeight:         r.GetCanaryWeight(),
			CurrentStep:          r.GetCurrentStep(),
			LatestAnalysisResult: r.GetLatestAnalysisResult(),
		})
	}
	if g := p.GetGateResult(); g != nil {
		gr := GateResult{Result: g.GetResult(), RanAt: g.GetRanAt()}
		for _, leg := range g.GetLegs() {
			gr.Legs = append(gr.Legs, GateLeg{
				Name:   leg.GetName(),
				Passed: leg.GetPassed(),
				Detail: leg.GetDetail(),
			})
		}
		out.GateResult = gr
	}
	return out
}

func nextVersionSuggestionFromProto(p *memqlv1.SuggestNextVersionResult) NextVersionSuggestion {
	if p == nil {
		return NextVersionSuggestion{}
	}
	return NextVersionSuggestion{
		CurrentVersion: p.GetCurrentVersion(),
		NextMajor:      p.GetNextMajor(),
		NextMinor:      p.GetNextMinor(),
		NextPatch:      p.GetNextPatch(),
		Source:         p.GetSource(),
	}
}

func actionResultFromProto(p *memqlv1.ActionResult) ActionResult {
	if p == nil {
		return ActionResult{}
	}
	return ActionResult{
		OK:            p.GetOk(),
		Message:       p.GetMessage(),
		AuditEventID:  p.GetAuditEventId(),
		CorrelationID: p.GetCorrelationId(),
		Details:       p.GetDetails(),
	}
}
