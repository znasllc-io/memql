package admin

import (
	"context"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// DeployControlReader is the narrow port the admin package uses to
// read per-env deployment status for the /admin/deployments view
// (memql#726). The wiring layer satisfies this with a thin adapter
// around the App's *deploycontrol.Service -- the handler never shells
// out to kubectl / argocd / git itself.
//
// The underlying read RPC is owner/admin-gated (#728), so the adapter
// (or its caller) must supply an actor context that resolves to the
// admin. handleDeploymentsGet stamps that context from the verified
// admin claims before invoking the reader; see deployActorContext.
type DeployControlReader interface {
	DeploymentStatus(ctx context.Context, env string) (*memqlv1.DeploymentStatus, error)
}

// SetDeployControlReader wires the deployment-status read dependency.
// Called once at bootstrap. When nil, the /admin/deployments route
// still mounts but renders an error flash rather than 500ing -- the
// page degrades gracefully on a binary built without the deploy-
// control surface.
func (s *AdminServer) SetDeployControlReader(r DeployControlReader) {
	if s == nil {
		return
	}
	s.deployReader = r
}

// deployActorContext stamps an auth.AccessContext built from the
// verified admin claims onto the context so the owner/admin-gated
// read RPC (#728) admits the in-process call. requireAdmin stamps
// only the claims + token (for the mutation pipeline's actor check),
// which the deploy-control service's resolveActor does NOT read; it
// reads auth.AccessFromContext / auth.UserFromContext. So we build
// the AccessContext explicitly from the same claims here.
func deployActorContext(ctx context.Context) context.Context {
	claims := claimsFromCtx(ctx)
	if claims == nil {
		return ctx
	}
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId:       claims.Subject,
		PrimaryEmail: claims.Email,
		Role:         auth.Role(strings.ToLower(strings.TrimSpace(claims.Role))),
	})
}

// handleDeploymentsGet renders /admin/deployments: the read-only
// per-env deployment view (overview + Argo CD + rollouts + gate).
// `?env=staging|prod` selects the environment (default staging).
// Read-only -- no write actions in this story (memql#726); write
// actions reuse this view + nav in the follow-up (memql#727).
func (s *AdminServer) handleDeploymentsGet(w http.ResponseWriter, r *http.Request) {
	env := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("env")))
	if env != "prod" {
		env = "staging"
	}

	data := webtempl.AdminDeploymentsData{
		Layout: s.layoutData(r, "Deployments", false),
		Flash:  readFlash(r),
		Env:    env,
	}

	if s.deployReader == nil {
		s.Logger.Warn("admin: deployments page requested but no deploy-control reader wired")
		data.Flash = &webtempl.Flash{Kind: "error", Message: "Deployment status is not available on this node."}
		s.render(w, r, "admin/deployments", webtempl.AdminDeployments(data))
		return
	}

	status, err := s.deployReader.DeploymentStatus(deployActorContext(r.Context()), env)
	if err != nil {
		s.Logger.Warn("admin: deployment status read failed", "error", err, "env", env)
		data.Flash = &webtempl.Flash{Kind: "error", Message: "Could not read deployment status: " + err.Error()}
		s.render(w, r, "admin/deployments", webtempl.AdminDeployments(data))
		return
	}

	data.Status = projectDeploymentStatus(status)
	s.render(w, r, "admin/deployments", webtempl.AdminDeployments(data))
}

// projectDeploymentStatus maps the generated DeploymentStatus proto
// into the templ-facing view struct (Go-internal -> templ-facing,
// mirroring projectUserView / projectAuditViews). Nil-safe on every
// nested message.
func projectDeploymentStatus(in *memqlv1.DeploymentStatus) webtempl.AdminDeploymentStatus {
	out := webtempl.AdminDeploymentStatus{
		Present:       in != nil,
		Env:           in.GetEnv(),
		Version:       in.GetVersion(),
		EngineVersion: in.GetEngineVersion(),
		ValidatedAt:   in.GetValidatedAt(),
		Gate:          in.GetGate(),
	}
	for _, c := range in.GetComponents() {
		out.Components = append(out.Components, webtempl.AdminDeployComponent{
			Name:   c.GetName(),
			Digest: c.GetDigest(),
			Repo:   c.GetRepo(),
		})
	}
	if argo := in.GetArgocd(); argo != nil {
		out.Argo = &webtempl.AdminDeployArgo{
			SyncStatus:       argo.GetSyncStatus(),
			HealthStatus:     argo.GetHealthStatus(),
			LastSyncRevision: argo.GetLastSyncRevision(),
			LastSyncAt:       argo.GetLastSyncAt(),
			OutOfSync:        argo.GetOutOfSync(),
		}
	}
	for _, ro := range in.GetRollouts() {
		out.Rollouts = append(out.Rollouts, webtempl.AdminDeployRollout{
			Name:                 ro.GetName(),
			Kind:                 ro.GetKind(),
			Phase:                ro.GetPhase(),
			ActiveColor:          ro.GetActiveColor(),
			PreviewColor:         ro.GetPreviewColor(),
			CanaryWeight:         int(ro.GetCanaryWeight()),
			CurrentStep:          int(ro.GetCurrentStep()),
			LatestAnalysisResult: ro.GetLatestAnalysisResult(),
		})
	}
	if gate := in.GetGateResult(); gate != nil {
		g := &webtempl.AdminDeployGate{
			Result: gate.GetResult(),
			RanAt:  gate.GetRanAt(),
		}
		for _, leg := range gate.GetLegs() {
			g.Legs = append(g.Legs, webtempl.AdminDeployGateLeg{
				Name:   leg.GetName(),
				Passed: leg.GetPassed(),
				Detail: leg.GetDetail(),
			})
		}
		out.GateResult = g
	}
	return out
}
