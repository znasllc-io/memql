package admin

import (
	"context"
	"net/http"
	"net/url"
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

// DeployControlActions is the narrow write port the admin package uses
// to drive the deploy-control write actions from the
// /admin/deployments POST handlers (memql#727). The wiring layer
// satisfies this with the same in-process adapter that backs
// DeployControlReader, forwarding to the owner/admin-gated +
// server-side-audited write RPCs on the *deploycontrol.Service.
//
// Each method returns the service's *memqlv1.ActionResult so the
// handler can surface the audit-event id in the success flash. As with
// the reader, the caller must stamp an actor context (deployActorContext)
// so the gated RPCs admit the in-process call.
type DeployControlActions interface {
	DeployStaging(ctx context.Context, version string) (*memqlv1.ActionResult, error)
	Promote(ctx context.Context, version string) (*memqlv1.ActionResult, error)
	Rollback(ctx context.Context, env, commitSha string) (*memqlv1.ActionResult, error)
	RolloutAction(ctx context.Context, env, rollout, action string) (*memqlv1.ActionResult, error)
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

// SetDeployControlActions wires the deployment write-action dependency
// (memql#727). Called once at bootstrap alongside SetDeployControlReader.
// When nil, the POST /admin/deployments/* routes still mount but reject
// with an error flash rather than 500ing -- the page degrades
// gracefully on a binary built without the deploy-control surface.
func (s *AdminServer) SetDeployControlActions(a DeployControlActions) {
	if s == nil {
		return
	}
	s.deployActions = a
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

// deployRedirect bounces back to the deployments view for the given
// env with a one-shot flash, mirroring the flash-redirect convention
// the other admin POST handlers use (?flash=...&flash_kind=...).
func deployRedirect(w http.ResponseWriter, r *http.Request, env, kind, msg string) {
	q := url.Values{}
	if env != "" {
		q.Set("env", env)
	}
	q.Set("flash", msg)
	q.Set("flash_kind", kind)
	http.Redirect(w, r, "/admin/deployments?"+q.Encode(), http.StatusSeeOther)
}

// deployActionResultFlash bounces back with a SUCCESS / ERROR flash
// derived from the deploy-control ActionResult. On a non-ok result the
// service-supplied message is surfaced; on ok the audit-event id is
// threaded into the success line so the operator has the audit anchor.
func deployActionResultFlash(w http.ResponseWriter, r *http.Request, env, verb string, res *memqlv1.ActionResult) {
	if res == nil {
		deployRedirect(w, r, env, "error", "ERROR: "+verb+" returned no result")
		return
	}
	if !res.GetOk() {
		msg := strings.TrimSpace(res.GetMessage())
		if msg == "" {
			msg = "action failed"
		}
		deployRedirect(w, r, env, "error", "ERROR: "+verb+": "+msg)
		return
	}
	deployRedirect(w, r, env, "success",
		"SUCCESS: "+verb+" (audit "+res.GetAuditEventId()+")")
}

// handleDeployStagingPost handles POST /admin/deployments/deploy-staging:
// assemble + apply a release into the staging overlay. Form: version.
// Staging is the non-destructive lane, so no type-to-confirm token.
func (s *AdminServer) handleDeployStagingPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		deployRedirect(w, r, "staging", "error", "ERROR: form submission failed")
		return
	}
	if s.deployActions == nil {
		deployRedirect(w, r, "staging", "error", "ERROR: deploy actions are not available on this node")
		return
	}
	version := strings.TrimSpace(r.PostForm.Get("version"))
	if version == "" {
		deployRedirect(w, r, "staging", "error", "ERROR: version is required")
		return
	}
	res, err := s.deployActions.DeployStaging(deployActorContext(r.Context()), version)
	if err != nil {
		s.Logger.Warn("admin: deploy-staging failed", "error", err, "version", version)
		deployRedirect(w, r, "staging", "error", "ERROR: deploy staging: "+err.Error())
		return
	}
	deployActionResultFlash(w, r, "staging", "deploy staging "+version, res)
}

// handleDeployPromotePost handles POST /admin/deployments/promote:
// copy a validated staging release into the prod overlay. Form:
// version, confirm. Destructive (touches prod) -- the operator must
// type the exact version into the confirm field or the action is
// rejected before it reaches the service.
func (s *AdminServer) handleDeployPromotePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		deployRedirect(w, r, "prod", "error", "ERROR: form submission failed")
		return
	}
	if s.deployActions == nil {
		deployRedirect(w, r, "prod", "error", "ERROR: deploy actions are not available on this node")
		return
	}
	version := strings.TrimSpace(r.PostForm.Get("version"))
	if version == "" {
		deployRedirect(w, r, "prod", "error", "ERROR: version is required")
		return
	}
	confirm := strings.TrimSpace(r.PostForm.Get("confirm"))
	if confirm != version {
		deployRedirect(w, r, "prod", "error",
			"ERROR: promote not confirmed -- type the exact version to confirm promotion to prod")
		return
	}
	res, err := s.deployActions.Promote(deployActorContext(r.Context()), version)
	if err != nil {
		s.Logger.Warn("admin: promote failed", "error", err, "version", version)
		deployRedirect(w, r, "prod", "error", "ERROR: promote: "+err.Error())
		return
	}
	deployActionResultFlash(w, r, "prod", "promote "+version+" to prod", res)
}

// handleDeployRollbackPost handles POST /admin/deployments/rollback:
// revert the overlay commit identified by commitSha for the given env.
// Form: env, commitSha, confirm. Destructive -- the operator must type
// either the exact commit SHA or the literal "rollback" into the
// confirm field.
func (s *AdminServer) handleDeployRollbackPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		deployRedirect(w, r, "staging", "error", "ERROR: form submission failed")
		return
	}
	env := normalizeDeployEnv(r.PostForm.Get("env"))
	if s.deployActions == nil {
		deployRedirect(w, r, env, "error", "ERROR: deploy actions are not available on this node")
		return
	}
	commitSha := strings.TrimSpace(r.PostForm.Get("commitSha"))
	if commitSha == "" {
		deployRedirect(w, r, env, "error", "ERROR: commit SHA is required")
		return
	}
	confirm := strings.TrimSpace(r.PostForm.Get("confirm"))
	if confirm != commitSha && confirm != "rollback" {
		deployRedirect(w, r, env, "error",
			"ERROR: rollback not confirmed -- type the commit SHA or the word rollback to confirm")
		return
	}
	res, err := s.deployActions.Rollback(deployActorContext(r.Context()), env, commitSha)
	if err != nil {
		s.Logger.Warn("admin: rollback failed", "error", err, "env", env, "commitSha", commitSha)
		deployRedirect(w, r, env, "error", "ERROR: rollback: "+err.Error())
		return
	}
	deployActionResultFlash(w, r, env, "rollback "+env+" to "+commitSha, res)
}

// handleDeployRolloutPost handles POST /admin/deployments/rollout:
// promote or abort an in-flight Argo Rollout. Form: env, rollout,
// action[, confirm]. "abort" is destructive (drops the new revision)
// so it requires the operator to type the literal "abort" into the
// confirm field; "promote" advances the rollout and needs no confirm.
func (s *AdminServer) handleDeployRolloutPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		deployRedirect(w, r, "staging", "error", "ERROR: form submission failed")
		return
	}
	env := normalizeDeployEnv(r.PostForm.Get("env"))
	if s.deployActions == nil {
		deployRedirect(w, r, env, "error", "ERROR: deploy actions are not available on this node")
		return
	}
	rollout := strings.TrimSpace(r.PostForm.Get("rollout"))
	if rollout == "" {
		deployRedirect(w, r, env, "error", "ERROR: rollout is required")
		return
	}
	action := strings.ToLower(strings.TrimSpace(r.PostForm.Get("action")))
	if action != "promote" && action != "abort" {
		deployRedirect(w, r, env, "error", "ERROR: action must be promote or abort")
		return
	}
	if action == "abort" {
		confirm := strings.ToLower(strings.TrimSpace(r.PostForm.Get("confirm")))
		if confirm != "abort" {
			deployRedirect(w, r, env, "error",
				"ERROR: abort not confirmed -- type the word abort to confirm aborting the rollout")
			return
		}
	}
	res, err := s.deployActions.RolloutAction(deployActorContext(r.Context()), env, rollout, action)
	if err != nil {
		s.Logger.Warn("admin: rollout action failed", "error", err, "env", env, "rollout", rollout, "action", action)
		deployRedirect(w, r, env, "error", "ERROR: rollout "+action+": "+err.Error())
		return
	}
	deployActionResultFlash(w, r, env, "rollout "+action+" "+rollout, res)
}

// normalizeDeployEnv mirrors handleDeploymentsGet's env normalization:
// anything other than "prod" collapses to "staging".
func normalizeDeployEnv(raw string) string {
	env := strings.ToLower(strings.TrimSpace(raw))
	if env != "prod" {
		env = "staging"
	}
	return env
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
