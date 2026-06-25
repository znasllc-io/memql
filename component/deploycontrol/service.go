// Package deploycontrol implements the gRPC DeployControlService behind
// the memQL Deployment Console (znasllc-io/memql#725 + #728).
//
// The service exposes the deployment-v2 machinery -- per-env image
// authority (the kustomize overlays), release lockfiles, Argo CD app
// status, Argo Rollouts progressive delivery, and the in-cluster
// deploy gate -- as a small set of unary RPCs.
//
// Read RPCs map raw `kubectl ... -o json` + on-disk overlay/lockfile
// state into typed status (pure mappers in status.go / overlay.go /
// lockfile.go). Write RPCs go through the sanctioned action scripts
// (scripts/release/promote.sh, `git revert`, `kubectl argo rollouts`)
// via the Executor boundary -- never ad-hoc `kubectl set image`.
//
// Every write RPC is gated to owner/admin (#728) and emits an audit
// event (success / failure / blocked). Read RPCs are NOT audited per
// call (too noisy).
package deploycontrol

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/core/id"
)

// validEnvs is the set of environments the console operates on.
var validEnvs = map[string]bool{"staging": true, "prod": true}

// Options configures NewService.
type Options struct {
	// Logger is required.
	Logger *slog.Logger
	// Audit is the identity AuditLogger that write actions + denials
	// emit to. Required.
	Audit identity.AuditLogger
	// RepoRoot anchors on-disk overlay/lockfile reads + action-script
	// invocation. Defaults to the current working directory when empty.
	RepoRoot string
	// Executor is the side-effect boundary. Defaults to the real
	// os/exec-backed executor anchored at RepoRoot when nil.
	Executor Executor
	// Clock is the time source (defaults to time.Now UTC).
	Clock func() time.Time
	// Engine, when set, persists deployments as v1:cluster:deployment
	// records (#1872): the write RPCs write a record at deploy start
	// (in_progress) and transition it (succeeded/failed) as the rollout
	// resolves. Optional -- nil leaves the deploy path unchanged (no
	// persistence), which is what unit tests + engineless binaries get.
	Engine identity.EngineExecutor
}

// Service implements memqlv1.DeployControlServiceServer.
type Service struct {
	memqlv1.UnimplementedDeployControlServiceServer

	logger   *slog.Logger
	audit    identity.AuditLogger
	repoRoot string
	exec     Executor
	clock    func() time.Time
	engine   identity.EngineExecutor
}

// NewService constructs the deploy-control service.
func NewService(opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, fmt.Errorf("deploycontrol: logger required")
	}
	if opts.Audit == nil {
		return nil, fmt.Errorf("deploycontrol: audit logger required")
	}
	repoRoot := opts.RepoRoot
	if repoRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			repoRoot = wd
		} else {
			repoRoot = "."
		}
	}
	executor := opts.Executor
	if executor == nil {
		executor = newExecExecutor(repoRoot)
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		logger:           opts.Logger,
		audit:            opts.Audit,
		repoRoot:         repoRoot,
		exec:             executor,
		clock:            clock,
		engine:           opts.Engine,
	}, nil
}

// Register attaches the DeployControlService implementation to the
// supplied gRPC server. Must be called BEFORE the server starts
// serving.
func (s *Service) Register(grpcServer *grpc.Server) {
	if s == nil || grpcServer == nil {
		return
	}
	memqlv1.RegisterDeployControlServiceServer(grpcServer, s)
}

// -----------------------------------------------------------------------------
// Actor resolution + owner/admin gate (#728)
// -----------------------------------------------------------------------------

// actor is the resolved caller identity used for the admin gate +
// audit attribution.
type actor struct {
	userID     string
	email      string
	role       auth.Role
	identityID string
}

func (a actor) userContext() auth.UserContext {
	return auth.UserContext{ID: a.userID, Email: a.email, Role: a.role}
}

// resolveActor pulls the caller's identity off the context. It prefers
// the AccessContext stamped by the gRPC auth middleware and falls back
// to the JWT-claims-derived UserContext. Fails closed (ok=false) when
// neither is present.
func resolveActor(ctx context.Context) (actor, bool) {
	if ac, ok := auth.AccessFromContext(ctx); ok {
		return actor{
			userID:     ac.UserId,
			email:      ac.PrimaryEmail,
			role:       ac.Role,
			identityID: ac.IdentityId,
		}, true
	}
	if uc, ok := auth.UserFromContext(ctx); ok {
		return actor{
			userID: uc.ID,
			email:  uc.Email,
			role:   uc.Role,
		}, true
	}
	return actor{}, false
}

// authorize resolves the caller and enforces the owner/admin gate
// (#728). On deny it emits a blocked audit event for the given verb +
// detail and returns a PermissionDenied status error. The returned
// actor is valid only when err == nil. This is the owner/admin gate for
// the legacy console actions (DeployStaging / Promote / Rollback /
// RolloutAction / GetDeploymentStatus); the #1876 actions use the
// looser authorizeDeploy (cut + deploy) or the stricter authorizeOwner
// (rollback_deployment).
func (s *Service) authorize(ctx context.Context, verb string, detail map[string]any) (actor, error) {
	return s.authorizeWith(ctx, verb, detail, auth.AtLeastAdmin, "owner or admin")
}

// authorizeDeploy enforces the developer-or-above gate for the
// forward-deploy actions -- cut version + deploy (memql#1876, epic
// #1871). Developer/admin/owner may ship; writer/reader stay
// read-only. Rollback is gated more strictly (owner-only) via
// authorizeOwner: developer (and admin) can deploy forward but not
// roll back.
func (s *Service) authorizeDeploy(ctx context.Context, verb string, detail map[string]any) (actor, error) {
	return s.authorizeWith(ctx, verb, detail, auth.AtLeastDeveloper, "developer, admin, or owner")
}

// authorizeOwner enforces the owner-only gate -- the strictest tier,
// reserved for rollback (memql#1876, per the locked role matrix). Not
// even admin may roll back; only the cluster owner can.
func (s *Service) authorizeOwner(ctx context.Context, verb string, detail map[string]any) (actor, error) {
	return s.authorizeWith(ctx, verb, detail, auth.IsOwner, "owner")
}

// authorizeWith is the shared gate body: resolve the caller, run the
// supplied role predicate, and on deny emit a blocked audit event for
// the verb + detail before returning a PermissionDenied (or
// Unauthenticated, when no actor is present) status error. The returned
// actor is valid only when err == nil.
func (s *Service) authorizeWith(
	ctx context.Context,
	verb string,
	detail map[string]any,
	allow func(auth.UserContext) bool,
	requirement string,
) (actor, error) {
	act, ok := resolveActor(ctx)
	if !ok {
		s.emitAudit(ctx, verb, actor{}, detail, identity.AuditOutcomeBlocked, "no authenticated actor")
		return actor{}, status.Error(codes.Unauthenticated, "deploy console: no authenticated caller")
	}
	if !allow(act.userContext()) {
		s.emitAudit(ctx, verb, act, detail, identity.AuditOutcomeBlocked, "caller role not permitted: "+string(act.role))
		return actor{}, status.Errorf(codes.PermissionDenied,
			"deploy console: %s requires %s role (have %q)", verb, requirement, act.role)
	}
	return act, nil
}

// -----------------------------------------------------------------------------
// Audit (#728)
// -----------------------------------------------------------------------------

// emitAudit logs one admin-category audit event for a write action /
// denial and returns the generated event id (which the caller threads
// into ActionResult.audit_event_id). Read paths do NOT call this.
func (s *Service) emitAudit(
	ctx context.Context,
	verb string,
	act actor,
	detail map[string]any,
	outcome identity.AuditOutcome,
	failureReason string,
) string {
	eventID := id.NewShortId()
	s.audit.Log(ctx, identity.AuditEvent{
		OccurredAt:    s.clock(),
		Category:      identity.AuditCategoryAdmin,
		Action:        "deployment_console_" + verb,
		ActorUserId:   act.userID,
		ActorEmail:    act.email,
		ActorRole:     string(act.role),
		ActorIdentity: act.identityID,
		TargetType:    "config",
		Detail:        detail,
		Outcome:       outcome,
		FailureReason: failureReason,
		CorrelationId: eventID,
	})
	return eventID
}

// -----------------------------------------------------------------------------
// Write RPCs
// -----------------------------------------------------------------------------

// DeployStaging assembles + applies a release into the staging overlay
// via promote.sh --env=staging.
func (s *Service) DeployStaging(ctx context.Context, req *memqlv1.DeployStagingRequest) (*memqlv1.ActionResult, error) {
	version := req.GetVersion()
	if version == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: version is required")
	}
	act, err := s.authorize(ctx, "deploy_staging", map[string]any{"env": "staging", "version": version})
	if err != nil {
		return nil, err
	}
	return s.promoteRelease(ctx, act, "deploy_staging", "staging", version), nil
}

// Promote copies a validated staging release into the prod overlay
// (digest copy, no rebuild) via promote.sh --env=prod.
func (s *Service) Promote(ctx context.Context, req *memqlv1.PromoteRequest) (*memqlv1.ActionResult, error) {
	version := req.GetVersion()
	if version == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: version is required")
	}
	act, err := s.authorize(ctx, "promote", map[string]any{"env": "prod", "version": version})
	if err != nil {
		return nil, err
	}
	return s.promoteRelease(ctx, act, "promote", "prod", version), nil
}

// promoteRelease is the shared record -> RunPromote -> transition ->
// audit sequence behind the two legacy console promote actions
// (DeployStaging env=staging, Promote env=prod). Extracting it (Epic 2 /
// E2.5, #2098) reduces deploycontrol toward the Executor effect boundary
// without changing behavior: the call order, the persisted deployment
// record (#1872; in_progress at start -> succeeded/failed on return), the
// promote.sh invocation via the SAME Executor.RunPromote, and the single
// audit event are byte-identical to the inlined form. The azure deploy
// path is unchanged.
func (s *Service) promoteRelease(ctx context.Context, act actor, verb, consoleEnv, version string) *memqlv1.ActionResult {
	// Persist the deployment record: write at deploy start (in_progress) so
	// a record exists even if the process dies mid-rollout, then transition
	// to succeeded/failed once promote.sh returns. Best-effort.
	deploymentID := s.recordDeployment(ctx, consoleEnv, version, s.resolveImageDigest(version), act)
	out, runErr := s.exec.RunPromote(ctx, version, consoleEnv)
	s.transitionDeployment(ctx, deploymentID, transitionStatusForErr(runErr))
	return s.finishWrite(ctx, verb, act, map[string]any{"env": consoleEnv, "version": version}, out, runErr,
		map[string]string{"env": consoleEnv, "version": version})
}

// Rollback reverts the overlay commit identified by commit_sha for the
// given environment via `git revert`.
func (s *Service) Rollback(ctx context.Context, req *memqlv1.RollbackRequest) (*memqlv1.ActionResult, error) {
	env := req.GetEnv()
	if !validEnvs[env] {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid env %q (want staging|prod)", env)
	}
	sha := req.GetCommitSha()
	if sha == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: commit_sha is required")
	}
	detail := map[string]any{"env": env, "commitSha": sha}
	act, err := s.authorize(ctx, "rollback", detail)
	if err != nil {
		return nil, err
	}
	out, runErr := s.exec.RunRollback(ctx, env, sha)
	return s.finishWrite(ctx, "rollback", act, detail, out, runErr,
		map[string]string{"env": env, "commitSha": sha}), nil
}

// RolloutAction promotes or aborts an in-flight Argo Rollout.
func (s *Service) RolloutAction(ctx context.Context, req *memqlv1.RolloutActionRequest) (*memqlv1.ActionResult, error) {
	env := req.GetEnv()
	if !validEnvs[env] {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid env %q (want staging|prod)", env)
	}
	rollout := req.GetRollout()
	if rollout == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: rollout is required")
	}
	action := req.GetAction()
	if action != "promote" && action != "abort" {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid action %q (want promote|abort)", action)
	}
	detail := map[string]any{"env": env, "rollout": rollout, "action": action}
	act, err := s.authorize(ctx, "rollout_action", detail)
	if err != nil {
		return nil, err
	}
	out, runErr := s.exec.RunRolloutAction(ctx, env, rollout, action)
	return s.finishWrite(ctx, "rollout_action", act, detail, out, runErr,
		map[string]string{"env": env, "rollout": rollout, "action": action}), nil
}

// finishWrite is the shared tail for every write RPC: emit exactly one
// audit event (success or failure) and build the ActionResult carrying
// the event id.
func (s *Service) finishWrite(
	ctx context.Context,
	verb string,
	act actor,
	detail map[string]any,
	output string,
	runErr error,
	resultDetails map[string]string,
) *memqlv1.ActionResult {
	ok := runErr == nil
	outcome := identity.AuditOutcomeSuccess
	failureReason := ""
	message := output
	if !ok {
		outcome = identity.AuditOutcomeFailure
		failureReason = runErr.Error()
		message = fmt.Sprintf("%s\n%s", runErr.Error(), output)
	}
	eventID := s.emitAudit(ctx, verb, act, detail, outcome, failureReason)
	return &memqlv1.ActionResult{
		Ok:            ok,
		Message:       message,
		AuditEventId:  eventID,
		CorrelationId: eventID,
		Details:       resultDetails,
	}
}

// -----------------------------------------------------------------------------
// Read RPC
// -----------------------------------------------------------------------------

// GetDeploymentStatus returns the aggregate per-env deployment view.
// Read-only; gated to owner/admin (#728). A successful read is NOT
// audited (reads stay un-audited on success); a denial emits a
// blocked audit event via authorize.
func (s *Service) GetDeploymentStatus(ctx context.Context, req *memqlv1.GetDeploymentStatusRequest) (*memqlv1.DeploymentStatus, error) {
	env := req.GetEnv()
	if !validEnvs[env] {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid env %q (want staging|prod)", env)
	}

	// Owner/admin gate (#728). The read RPC is the API read-gate the
	// console's portal view rides on; deny non-admins with a blocked
	// audit event. Successful reads are not audited.
	if _, err := s.authorize(ctx, "get_status", map[string]any{"env": env}); err != nil {
		return nil, err
	}

	out := &memqlv1.DeploymentStatus{Env: env}

	// Overlay (on-disk image authority) -> components + promoted version.
	overlayPath := filepath.Join(s.repoRoot, "deploy", "k8s", "overlays", env, "kustomization.yaml")
	overlayRaw, err := os.ReadFile(overlayPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deploy console: read overlay %s: %v", overlayPath, err)
	}
	overlay, err := ParseOverlay(overlayRaw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deploy console: %v", err)
	}
	out.Version = overlay.PromotedVersion

	// Resolve the lockfile when the overlay names a promoted version
	// (prod). Staging carries no promotion provenance, so engineVersion
	// / validatedAt / gate / repo stay empty unless a lockfile is found.
	var lockfile *Lockfile
	if overlay.PromotedVersion != "" {
		lfPath := filepath.Join(s.repoRoot, "releases", overlay.PromotedVersion+".yaml")
		if lfRaw, lfErr := os.ReadFile(lfPath); lfErr == nil {
			if lf, perr := ParseLockfile(lfRaw); perr == nil {
				lockfile = &lf
				out.EngineVersion = lf.EngineVersion
				out.ValidatedAt = lf.ValidatedAt
				out.Gate = lf.Gate
			}
		}
	}

	for _, img := range overlay.Images {
		repo := ""
		if lockfile != nil {
			if c, found := lockfile.Components[img.ShortName()]; found {
				repo = c.Repo
			}
		}
		out.Components = append(out.Components, &memqlv1.ComponentDigest{
			Name:   img.Name,
			Digest: img.Digest,
			Repo:   repo,
		})
	}

	// Argo CD app status (best-effort: a kubectl failure leaves argocd nil).
	if argoRaw, aerr := s.exec.KubectlJSON(ctx, "-n", "argocd", "get", "app", "memql", "-o", "json"); aerr == nil {
		if argo, merr := MapArgoStatus(argoRaw); merr == nil {
			out.Argocd = argo
		}
	}

	// Rollouts.
	if rolloutsRaw, rerr := s.exec.KubectlJSON(ctx, "-n", "memql", "get", "rollout", "-o", "json"); rerr == nil {
		if rollouts, merr := MapRolloutList(rolloutsRaw); merr == nil {
			out.Rollouts = rollouts
		}
	}

	// Deploy gate (latest AnalysisRun).
	if gateRaw, gerr := s.exec.KubectlJSON(ctx, "-n", "memql", "get", "analysisrun", "-o", "json"); gerr == nil {
		if gate, merr := MapGateResult(gateRaw); merr == nil {
			out.GateResult = gate
		}
	} else {
		out.GateResult = &memqlv1.GateResult{Result: "unknown"}
	}

	return out, nil
}
