// Package deploycontrol implements the gRPC DeployControlService behind
// the MemQL Deployment Console (znasllc-io/memql#725 + #728).
//
// The service exposes the deployment-v2 machinery -- the image authority
// (the kustomize overlay), release lockfiles, Argo CD app status, Argo
// Rollouts progressive delivery, and the in-cluster deploy gate -- as a
// small set of unary RPCs.
//
// Read RPCs map raw `kubectl ... -o json` + on-disk overlay/lockfile
// state into typed status (pure mappers in status.go / overlay.go /
// lockfile.go). Write RPCs go through the sanctioned actions (`git revert`,
// `kubectl argo rollouts`) via the Executor boundary -- never ad-hoc
// `kubectl set image`.
//
// Every RPC operates on THIS installation and takes no environment (epic
// memql#3943). Where the cluster is addressed, it is addressed by the three
// constants below.
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

// The console addresses ONE installation, and these three constants are its
// whole address: the namespace the workloads run in, the ArgoCD Application
// that reconciles them, and the kustomize overlay that is their image
// authority.
//
// They were a per-environment lookup table until epic memql#3943 removed
// "environment" as a product concept. An operator who wants a second
// environment installs a second instance, which has its own namespace, its own
// Application and its own overlay -- so there is nothing here for a caller to
// select between, and no parameter to get wrong.
//
// The namespace and the Application share a name, and that is a coincidence
// rather than a derivation: they answer different questions ("where do the pods
// live" versus "what does an operator sync"), and an estate that renames one
// has no reason to rename the other.
const (
	clusterNamespace = "memql"
	argoApplication  = "memql"
)

// overlayDir is the cloud kustomize overlay, relative to the repo root. The
// local k3d overlay is deliberately not addressable here: it is not a deploy
// target, and validateDeploymentProvider (driver.go) refuses the retired
// `docker-local` provider with a pointer to `make up`.
var overlayDir = filepath.Join("deploy", "k8s", "overlays", "cloud")

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
		logger:   opts.Logger,
		audit:    opts.Audit,
		repoRoot: repoRoot,
		exec:     executor,
		clock:    clock,
		engine:   opts.Engine,
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
// the legacy console actions (Rollback / RolloutAction /
// GetDeploymentStatus); the #1876 actions use the looser authorizeDeploy
// (cut + deploy) or the stricter authorizeOwner (rollback_deployment).
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
//
// The refusal carries the blocked event's correlation id back to the caller
// as a RefusalInfo detail (memql#3334). emitAudit has always returned that id
// and this function used to discard it, which made the one event an operator
// most wants to quote -- their own denial -- the only one with no reference.
// See refusal.go for why handing it to the refused caller is safe and why the
// alternative (admin-only visibility) was rejected.
func (s *Service) authorizeWith(
	ctx context.Context,
	verb string,
	detail map[string]any,
	allow func(auth.UserContext) bool,
	requirement string,
) (actor, error) {
	act, ok := resolveActor(ctx)
	if !ok {
		eventID := s.emitAudit(ctx, verb, actor{}, detail, identity.AuditOutcomeBlocked, "no authenticated actor")
		return actor{}, refusalError(codes.Unauthenticated, eventID, "deploy console: no authenticated caller")
	}
	if !allow(act.userContext()) {
		eventID := s.emitAudit(ctx, verb, act, detail, identity.AuditOutcomeBlocked, "caller role not permitted: "+string(act.role))
		return actor{}, refusalError(codes.PermissionDenied, eventID, fmt.Sprintf(
			"deploy console: %s requires %s role (have %q)", verb, requirement, act.role))
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

// GATE BEFORE VALIDATION (memql#3457, completed across the surface by
// memql#3505). EVERY RPC on this service -- the seven writes and the two reads,
// wherever they live in the package -- calls the role gate FIRST and validates
// its arguments only once the caller has cleared it. The order used to be the
// other way round, which meant
// a below-floor caller who sent a bad argument received a plain
// INVALID_ARGUMENT and left NO audit event -- the one hole in the "every
// refused deploy action is audited" property memql#3334 established and pinned.
// The attempt achieved nothing (the argument was invalid, so no deployment
// moved), which is why it was easy to leave alone; but an audit trail on a
// privileged surface exists to record ATTEMPTS, and it was invisible from both
// ends -- the caller read an argument error, the admin reading the trail read
// nothing.
//
// The re-order is deliberately observable: a below-floor caller with an invalid
// argument now gets PERMISSION_DENIED (audited) where it used to get
// INVALID_ARGUMENT (unaudited). A caller at or above the floor is unaffected --
// same code, same message, still unaudited, because the gate audits denials and
// not admissions. It is also the safer order on its own terms: an unauthorized
// caller learns nothing from the shape of the argument parser.
//
// The audit detail map therefore carries the argument AS SENT, invalid values
// included. That is the point -- the shape of a probe is the interesting part
// of it.
//
// TestBelowFloorCallerWithAnInvalidArgumentIsRefusedAndAudited (unary +
// streamed) and TestForwardedBelowFloorCallerWithAnInvalidArgumentIsRefusedAndAudited
// (over the memql#3380 mesh hop) hold this on all three surfaces, for every
// role each RPC denies -- not for `reader` alone, because below-the-floor is a
// per-action fact (developer may cut and deploy; admin may do everything except
// roll a deployment back). TestBelowFloorInvalidArgumentCoverage derives the
// required set from the generated interface, so an RPC added later cannot
// re-open the hole by being forgotten.

// Rollback reverts the overlay commit identified by commit_sha via
// `git revert`.
func (s *Service) Rollback(ctx context.Context, req *memqlv1.RollbackRequest) (*memqlv1.ActionResult, error) {
	sha := req.GetCommitSha()
	detail := map[string]any{"commitSha": sha}
	act, err := s.authorize(ctx, "rollback", detail)
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: commit_sha is required")
	}
	out, runErr := s.exec.RunRollback(ctx, sha)
	return s.finishWrite(ctx, "rollback", act, detail, out, runErr,
		map[string]string{"commitSha": sha}), nil
}

// RolloutAction promotes or aborts an in-flight Argo Rollout.
func (s *Service) RolloutAction(ctx context.Context, req *memqlv1.RolloutActionRequest) (*memqlv1.ActionResult, error) {
	rollout := req.GetRollout()
	action := req.GetAction()
	detail := map[string]any{"rollout": rollout, "action": action}
	act, err := s.authorize(ctx, "rollout_action", detail)
	if err != nil {
		return nil, err
	}
	if rollout == "" {
		return nil, status.Error(codes.InvalidArgument, "deploy console: rollout is required")
	}
	if action != "promote" && action != "abort" {
		return nil, status.Errorf(codes.InvalidArgument, "deploy console: invalid action %q (want promote|abort)", action)
	}
	out, runErr := s.exec.RunRolloutAction(ctx, rollout, action)
	return s.finishWrite(ctx, "rollout_action", act, detail, out, runErr,
		map[string]string{"rollout": rollout, "action": action}), nil
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

// GetDeploymentStatus returns the aggregate deployment view for this
// installation. Read-only; gated to owner/admin (#728). A successful read is
// NOT audited (reads stay un-audited on success); a denial emits a blocked
// audit event via authorize.
func (s *Service) GetDeploymentStatus(ctx context.Context, _ *memqlv1.GetDeploymentStatusRequest) (*memqlv1.DeploymentStatus, error) {
	// Owner/admin gate (#728). The read RPC is the API read-gate the console's
	// portal view rides on; deny non-admins with a blocked audit event.
	// Successful reads are not audited.
	if _, err := s.authorize(ctx, "get_status", map[string]any{}); err != nil {
		return nil, err
	}

	out := &memqlv1.DeploymentStatus{}

	// Overlay (on-disk image authority) -> components + promoted version.
	overlayPath := filepath.Join(s.repoRoot, overlayDir, "kustomization.yaml")
	overlayRaw, err := os.ReadFile(overlayPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deploy console: read overlay %s: %v", overlayPath, err)
	}
	overlay, err := ParseOverlay(overlayRaw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deploy console: %v", err)
	}
	out.Version = overlay.PromotedVersion

	// Resolve the lockfile when the overlay names a promoted version. An
	// overlay that carries no promotion provenance leaves engineVersion /
	// validatedAt / gate / repo empty.
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
	if argoRaw, aerr := s.exec.KubectlJSON(ctx, "-n", "argocd", "get", "app", argoApplication, "-o", "json"); aerr == nil {
		if argo, merr := MapArgoStatus(argoRaw); merr == nil {
			out.Argocd = argo
		}
	}

	// Rollouts. Best-effort in the sense that a kubectl FAILURE leaves the field
	// nil -- but note that a wrong namespace is not a failure: it is an empty
	// list and exit 0, which is a healthy-looking answer for an address nothing
	// is at. That is why the namespace is a named constant rather than a value
	// threaded in from a caller (see clusterNamespace above).
	if rolloutsRaw, rerr := s.exec.KubectlJSON(ctx, "-n", clusterNamespace, "get", "rollout", "-o", "json"); rerr == nil {
		if rollouts, merr := MapRolloutList(rolloutsRaw); merr == nil {
			out.Rollouts = rollouts
		}
	}

	// Deploy gate (latest AnalysisRun).
	if gateRaw, gerr := s.exec.KubectlJSON(ctx, "-n", clusterNamespace, "get", "analysisrun", "-o", "json"); gerr == nil {
		if gate, merr := MapGateResult(gateRaw); merr == nil {
			out.GateResult = gate
		}
	} else {
		out.GateResult = &memqlv1.GateResult{Result: "unknown"}
	}

	return out, nil
}
