package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	workstate "github.com/znasllc-io/memql/component/work"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// Every replica binds the same durable request journal. Incoming requests can
// name a visible source; only the installed resolver may select a maintainer's
// source and only this adapter restores its persisted identity.
func (a *App) wireRepositoryAuthority() {
	if a.engine == nil {
		return
	}
	provider, ok := a.engine.IntegrationByName("repositoryauthority").(*repositoryauthority.Integration)
	if !ok || provider == nil {
		return
	}
	pkgs := a.lookupPackagesIntegration()
	provider.Configure(func(d *repositoryauthority.Deps) {
		if db := a.directDBGetter()(); db != nil {
			d.DB = db.DB
		}
		d.Source = a.repositoryAuthoritySource
		d.SystemVariable = a.engine.ResolveSystemVariable
		d.RepositoryAccess = func(ctx context.Context, s repositoryauthority.Source) (packages.RepositoryAuthority, error) {
			if pkgs == nil {
				return packages.RepositoryAuthority{}, errors.New("repository connection is unavailable")
			}
			parts := strings.Split(s.Repository, "/")
			if len(parts) != 2 {
				return packages.RepositoryAuthority{}, errors.New("repository identity is invalid")
			}
			return pkgs.RepositoryAccess(ctx, s.CredentialID, s.OwnerUserID, parts[0], parts[1])
		}
		d.OpenGoal = a.openRepositoryAuthorityGoal
		d.Decision = a.repositoryAuthorityDecision
		d.ExecutionActive = a.repositoryAuthorityExecutionActive
		d.RemoteBinding = a.repositoryAuthorityRemoteBinding
		d.RemoteRequest = a.repositoryAuthorityRemoteRequest
		d.RemoteStatus = a.repositoryAuthorityRemoteStatus
		d.CaptureReview = func(_ context.Context, name string) (map[string]any, error) { return a.engine.CapturePrompt(name) }
		d.Review = a.reviewRepositoryChange
		d.DeliveryContext = a.repositoryAuthorityDeliveryContext
		d.ReportMergeGroup = func(ctx context.Context, intent repositoryauthority.MergeGroupReviewIntent, authorize func(context.Context) error) (repositoryauthority.MergeGroupReviewReceipt, error) {
			if pkgs == nil {
				return repositoryauthority.MergeGroupReviewReceipt{}, errors.New("repository connection is unavailable")
			}
			return reportRepositoryMergeGroup(ctx, pkgs, intent, authorize)
		}
		d.ReportReview = func(ctx context.Context, grant repositoryauthority.Authorization, intent repositoryauthority.ReviewReportIntent) (repositoryauthority.ReviewReportReceipt, error) {
			if pkgs == nil {
				return repositoryauthority.ReviewReportReceipt{}, errors.New("repository connection is unavailable")
			}
			return reportRepositoryReview(ctx, pkgs, provider, grant, intent)
		}
		d.Queue = func(ctx context.Context, grant repositoryauthority.Authorization, intent repositoryauthority.DeliveryIntent) (repositoryauthority.DeliveryReceipt, error) {
			if pkgs == nil {
				return repositoryauthority.DeliveryReceipt{}, errors.New("repository connection is unavailable")
			}
			return queueRepositoryChange(ctx, pkgs, provider, grant, intent)
		}
		d.Read = func(ctx context.Context, grant repositoryauthority.Authorization, paths []string) (repositoryauthority.RepositorySnapshot, error) {
			if pkgs == nil {
				return repositoryauthority.RepositorySnapshot{}, errors.New("repository connection is unavailable")
			}
			return readRepositoryChange(ctx, pkgs, grant, paths)
		}
		d.Propose = func(ctx context.Context, grant repositoryauthority.Authorization, proposal repositoryauthority.Proposal) (repositoryauthority.ProposalReceipt, error) {
			if pkgs == nil {
				return repositoryauthority.ProposalReceipt{}, errors.New("repository connection is unavailable")
			}
			return proposeRepositoryChange(ctx, pkgs, provider, grant, proposal)
		}
	})
	if work, ok := a.engine.IntegrationByName("work").(*workspine.Integration); ok && work != nil {
		work.SetPlanReviewValidator(repositoryauthority.ReviewType, provider.ValidateReview)
	}
}

func (a *App) repositoryAuthoritySource(ctx context.Context, sourceID string, trustedBinding bool) (repositoryauthority.Source, error) {
	var out repositoryauthority.Source
	if sourceID == "" {
		return out, errors.New("repository source is required")
	}
	readCtx := memql.ContextWithFreshRead(ctx)
	if trustedBinding {
		// Only IDs selected by installed operator policy reach this branch.
		// No incoming owner, credential or provider permission is accepted.
		readCtx = memql.ContextWithFreshRead(auth.ContextWithSystemActor(ctx, "repository-authority"))
	}
	rows, err := workTemplateRows(readCtx, a.engine, "packageById", map[string]any{"packageId": sourceID})
	if err != nil {
		return out, err
	}
	if len(rows) != 1 {
		return out, errors.New("repository source is unavailable to this requester")
	}
	r := rows[0]
	if authorityString(r, "sourceKind") != "repo" || authorityString(r, "status") != "active" || r["sourceRemoved"] == true {
		return out, errors.New("repository source is inactive")
	}
	u, err := url.Parse(authorityString(r, "repoUrl"))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return out, errors.New("repository source needs its exact GitHub URL")
	}
	repo := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return out, errors.New("repository source identity is invalid")
	}
	out = repositoryauthority.Source{ID: memql.BareShortId(authorityString(r, "id")), OwnerUserID: memql.BareShortId(authorityString(r, "ownerUserId")), CredentialID: memql.BareShortId(authorityString(r, "credentialId")), Repository: strings.ToLower(repo)}
	if out.ID != memql.BareShortId(sourceID) || out.OwnerUserID == "" || out.CredentialID == "" {
		return repositoryauthority.Source{}, errors.New("repository source lacks an owner-bound connection")
	}
	return out, nil
}

func authorityString(row map[string]any, key string) string { s, _ := row[key].(string); return s }

// Effects must observe cancellation on the replica which received it, even
// while an older executing replica still holds a live RunContext.
func (a *App) repositoryAuthorityExecutionActive(ctx context.Context, owner, goalID, runID string) error {
	if owner == "" || goalID == "" || runID == "" {
		return errors.New("repository execution identity is missing")
	}
	read := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, owner))
	runs, err := workTemplateRows(read, a.engine, "workRunForOwner", map[string]any{"runId": runID})
	if err != nil {
		return err
	}
	if len(runs) != 1 {
		return errors.New("repository execution is unavailable")
	}
	r := runs[0]
	if memql.BareShortId(authorityString(r, "id")) != memql.BareShortId(runID) || memql.BareShortId(authorityString(r, "ownerUserId")) != memql.BareShortId(owner) || memql.BareShortId(authorityString(r, "goalId")) != memql.BareShortId(goalID) || authorityString(r, "mode") != "live" || authorityString(r, "status") != "running" || r["cancelRequested"] == true {
		return errors.New("repository execution is no longer active")
	}
	goals, err := workTemplateRows(read, a.engine, "workGoalForOwner", map[string]any{"goalId": goalID})
	if err != nil {
		return err
	}
	if len(goals) != 1 || memql.BareShortId(authorityString(goals[0], "id")) != memql.BareShortId(goalID) || memql.BareShortId(authorityString(goals[0], "ownerUserId")) != memql.BareShortId(owner) || (authorityString(goals[0], "status") != "open" && authorityString(goals[0], "status") != "active") {
		return errors.New("repository goal is no longer open")
	}
	return nil
}

func (a *App) openRepositoryAuthorityGoal(ctx context.Context, r repositoryauthority.GoalRequest) (repositoryauthority.GoalReceipt, error) {
	var out repositoryauthority.GoalReceipt
	work, ok := a.engine.IntegrationByName("work").(*workspine.Integration)
	if !ok || work == nil {
		return out, errors.New("repository change goal service is unavailable")
	}
	actor, _ := auth.AccessFromContext(ctx)
	if actor == nil || memql.BareShortId(actor.UserId) != memql.BareShortId(r.OwnerUserID) {
		var err error
		ctx, err = a.repositoryMaintainerContext(ctx, r.OwnerUserID)
		if err != nil {
			return out, err
		}
	}
	g := workspine.DirectGoal{OwnerUserId: r.OwnerUserID, Statement: r.Statement, AutomationName: r.Template, Input: r.Input, RequestedVia: "pipeline", TriggeredBy: "repository-change", BeforeRun: r.BeforeRun}
	var receipt workspine.ReviewGoalReceipt
	var err error
	if r.Immediate {
		receipt, err = work.OpenAnalysisGoal(ctx, r.Key, g)
	} else {
		receipt, err = work.OpenReviewGoal(ctx, r.Key, g, r.Subject, "Allow this repository change?")
	}
	return repositoryauthority.GoalReceipt{GoalID: receipt.GoalID, RunID: receipt.RunID, ApprovalID: receipt.ApprovalID}, err
}

// The native authority journal selected this owner from a trusted source and
// checked its live provider permission. Resolve the actual local identity;
// never borrow an incoming host's asserted role or credential ceiling.
func (a *App) repositoryMaintainerContext(ctx context.Context, owner string) (context.Context, error) {
	if owner == "" {
		return nil, errors.New("maintainer identity is missing")
	}
	resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(c context.Context, q string) (any, error) {
		r, err := a.engine.Execute(c, q)
		if err != nil || r == nil {
			return nil, err
		}
		return r.OutputPayload(), nil
	}), a.Logger)
	canonical := "v1:identity:user:" + memql.BareShortId(owner)
	access, err := resolver.LoadFromClaims(memql.ContextWithFreshRead(ctx), map[string]any{"sub": canonical})
	if err != nil || access == nil || !auth.IsValidRole(access.Role) {
		return nil, errors.New("maintainer identity is unavailable")
	}
	bound, err := auth.ContextWithPersistedOwner(ctx, canonical, map[string]any{"roleCeiling": string(access.Role), "credentialClass": auth.ForwardedClassUser}, resolver)
	if err != nil {
		return nil, err
	}
	forwarded, ok := auth.ForwardedAuthorityFromContext(bound)
	if !ok {
		return nil, errors.New("maintainer authority could not be restored")
	}
	return auth.ContextWithExecutionAuthority(bound, forwarded, nil), nil
}

func (a *App) repositoryAuthorityDecision(ctx context.Context, owner, approvalID string) (repositoryauthority.Decision, error) {
	var out repositoryauthority.Decision
	if owner == "" || approvalID == "" {
		return out, errors.New("repository approval identity is missing")
	}
	read := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, owner))
	rows, err := workTemplateRows(read, a.engine, "workApprovalForOwner", map[string]any{"approvalId": approvalID})
	if err != nil {
		return out, err
	}
	if len(rows) != 1 {
		return out, errors.New("repository approval is unavailable")
	}
	r := rows[0]
	subject, _ := r["subject"].(map[string]any)
	if memql.BareShortId(authorityString(r, "id")) != memql.BareShortId(approvalID) || memql.BareShortId(authorityString(r, "ownerUserId")) != memql.BareShortId(owner) || authorityString(r, "kind") != workstate.ApprovalKindPlanReview || authorityString(subject, "reviewType") != repositoryauthority.ReviewType || authorityString(subject, "requestId") == "" || workstate.ArtifactHash(subject) != authorityString(r, "artifactHash") || authorityString(r, "permissionRevokedAt") != "" {
		return out, errors.New("repository approval does not match its recorded request")
	}
	if value := authorityString(r, "expiresAt"); value != "" {
		expiry, e := time.Parse(time.RFC3339Nano, value)
		if e != nil || !time.Now().Before(expiry) {
			return out, errors.New("repository approval expired")
		}
	}
	runs, err := workTemplateRows(read, a.engine, "workRunForOwner", map[string]any{"runId": authorityString(r, "runId")})
	if err != nil {
		return out, err
	}
	if len(runs) != 1 || memql.BareShortId(authorityString(runs[0], "ownerUserId")) != memql.BareShortId(owner) {
		return out, errors.New("repository approval run is unavailable")
	}
	out.GoalID = memql.BareShortId(authorityString(runs[0], "goalId"))
	out.RunID = memql.BareShortId(authorityString(runs[0], "id"))
	if out.GoalID == "" || out.RunID != memql.BareShortId(authorityString(r, "runId")) {
		return out, errors.New("repository approval run identity differs")
	}
	switch authorityString(r, "decision") {
	case "":
		out.State = "pending"
	case "approved":
		out.State = "approved"
	case "rejected":
		out.State = "declined"
	default:
		return out, errors.New("repository approval has no usable decision")
	}
	if out.State != "pending" && (authorityString(r, "decidedAt") == "" || memql.BareShortId(authorityString(r, "decidedBy")) != memql.BareShortId(owner)) {
		return repositoryauthority.Decision{}, errors.New("repository approval lacks its maintainer decision")
	}
	answer, _ := r["answer"].(map[string]any)
	out.Reason = authorityString(answer, "reason")
	out.RequestID = authorityString(subject, "requestId")
	out.Fingerprint = authorityString(subject, "fingerprint")
	return out, nil
}
