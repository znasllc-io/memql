package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	"github.com/znasllc-io/memql/core/common"
)

type repositoryQueueAccess interface {
	GitHub() *githubapp.Client
	RepositoryQueueObservationToken(context.Context, string, string, string, string) (string, int64, error)
	RepositoryQueueUserToken(context.Context, string, string, string, string) (string, int64, error)
}
type repositoryDeliveryAuthority interface {
	AuthorizeDelivery(context.Context, string) (repositoryauthority.Authorization, error)
}

func queueRepositoryChange(ctx context.Context, access repositoryQueueAccess, authority repositoryDeliveryAuthority, grant repositoryauthority.Authorization, intent repositoryauthority.DeliveryIntent) (repositoryauthority.DeliveryReceipt, error) {
	var out repositoryauthority.DeliveryReceipt
	parts := strings.Split(grant.Source.Repository, "/")
	if access == nil || access.GitHub() == nil || authority == nil || len(parts) != 2 || grant.RepositoryID <= 0 || grant.Source.CredentialID == "" || grant.Source.OwnerUserID == "" || grant.EffectID == "" || intent.ProposalHash != grant.EffectID || intent.Proposal.Branch != "memql/change-"+intent.ProposalHash {
		return out, errors.New("queue admission has no exact native proposal intent")
	}
	u, err := url.Parse(intent.Proposal.PullRequestURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return out, errors.New("queue proposal has no exact GitHub pull request URL")
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segments) != 4 || !strings.EqualFold(segments[0]+"/"+segments[1], grant.Source.Repository) || segments[2] != "pull" {
		return out, errors.New("queue proposal names another repository")
	}
	number, err := strconv.Atoi(segments[3])
	if err != nil || number <= 0 || strconv.Itoa(number) != segments[3] {
		return out, errors.New("queue proposal has an invalid pull request identity")
	}
	check := func(c context.Context) error {
		current, err := authority.AuthorizeDelivery(c, grant.RequestID)
		if err != nil {
			return err
		}
		if current.RepositoryID != grant.RepositoryID || current.Source != grant.Source || current.EffectID != grant.EffectID {
			return errors.New("repository delivery authority changed")
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return out, err
	}
	observation, _, err := access.RepositoryQueueObservationToken(ctx, grant.Source.CredentialID, grant.Source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return out, err
	}
	gh := access.GitHub()
	info, err := gh.Repository(ctx, observation, parts[0], parts[1])
	if err != nil {
		return out, err
	}
	if info.ID != grant.RepositoryID || !strings.EqualFold(info.FullName, grant.Source.Repository) || info.DefaultBranch != intent.BaseBranch {
		return out, errors.New("repository delivery identity changed")
	}
	refresh := func(c context.Context) (string, error) {
		if err := check(c); err != nil {
			return "", err
		}
		// The same observation credential must still identify the original
		// provider repository before each effect, including name reuse races.
		current, err := gh.Repository(c, observation, parts[0], parts[1])
		if err != nil {
			return "", err
		}
		if current.ID != grant.RepositoryID || !strings.EqualFold(current.FullName, grant.Source.Repository) || current.DefaultBranch != intent.BaseBranch {
			return "", errors.New("repository delivery identity changed")
		}
		if err := check(c); err != nil {
			return "", err
		}
		token, _, err := access.RepositoryQueueUserToken(c, grant.Source.CredentialID, grant.Source.OwnerUserID, parts[0], parts[1])
		if err != nil {
			return "", err
		}
		if err := check(c); err != nil {
			return "", err
		}
		return token, nil
	}
	var result githubapp.QueueAdmission
	if intent.ObserveOnly {
		result, err = gh.ObserveReviewedChange(ctx, observation, parts[0], parts[1], number, intent.Proposal.Commit, intent.BaseBranch)
	} else {
		result, err = gh.QueueReviewedChange(ctx, observation, parts[0], parts[1], number, intent.Proposal.Commit, intent.BaseBranch, refresh)
	}
	return repositoryauthority.DeliveryReceipt{State: result.State, Reason: result.Reason, HeadSHA: result.HeadSHA, QueueEntryID: result.QueueEntryID}, err
}

func (a *App) reviewRepositoryChange(ctx context.Context, _ repositoryauthority.Authorization, snapshot map[string]any, input repositoryauthority.ReviewInput) (repositoryauthority.ReviewResult, error) {
	var result repositoryauthority.ReviewResult
	jsonText := func(value any) (string, error) { data, err := json.Marshal(value); return string(data), err }
	paths, err := jsonText(input.Paths)
	if err != nil {
		return result, err
	}
	source, err := jsonText(input.Source)
	if err != nil {
		return result, err
	}
	proposal, err := jsonText(input.Proposal)
	if err != nil {
		return result, err
	}
	data := map[string]any{"statement": input.Statement, "repository": input.Repository, "operation": input.Operation, "paths": paths, "source": source, "proposal": proposal}
	call, err := a.engine.InvokeCapturedPromptStructured(repositoryIndependentReviewContext(ctx), snapshot, data, "repository_review", `{"type":"object","additionalProperties":false,"required":["approved","reason"],"properties":{"approved":{"type":"boolean"},"reason":{"type":"string","minLength":1,"maxLength":4000}}}`, true)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(call.Text), &result); err != nil {
		return result, err
	}
	return result, nil
}

func repositoryIndependentReviewContext(ctx context.Context) context.Context {
	// A draft author's rerun instruction is not part of the independently
	// approved reviewer. Keep journal identity, ceilings and cancellation.
	if run, ok := common.RunFromContext(ctx); ok {
		run.Override = nil
		ctx = common.ContextWithRun(ctx, run)
	}
	return ctx
}

// The native queue recovery lease grants only observation/admission of the
// original reviewed PR. It cannot reactivate proposal effects after success.
func (a *App) repositoryAuthorityDeliveryContext(ctx context.Context, owner, goalID, runID string) (context.Context, error) {
	if owner == "" || goalID == "" || runID == "" {
		return nil, errors.New("repository delivery identity is missing")
	}
	bound, err := a.repositoryMaintainerContext(ctx, owner)
	if err != nil {
		return nil, err
	}
	read := memql.ContextWithFreshRead(bound)
	j, err := automations.LoadRunJournal(read, a.engine, runID)
	if err != nil {
		return nil, err
	}
	if memql.BareShortId(j.RunId) != memql.BareShortId(runID) || memql.BareShortId(j.OwnerUserId) != memql.BareShortId(owner) || memql.BareShortId(j.GoalId) != memql.BareShortId(goalID) || j.Mode != common.RunModeLive || (j.Status != "running" && j.Status != "succeeded") || j.CancelRequested || j.ForkedFromRunId != "" || j.TriggeredBy != "repository-change" {
		return nil, errors.New("repository delivery run is no longer eligible")
	}
	goals, err := workTemplateRows(read, a.engine, "workGoalForOwner", map[string]any{"goalId": goalID})
	if err != nil {
		return nil, err
	}
	if len(goals) != 1 || memql.BareShortId(authorityString(goals[0], "id")) != memql.BareShortId(goalID) || memql.BareShortId(authorityString(goals[0], "ownerUserId")) != memql.BareShortId(owner) || (authorityString(goals[0], "status") != "open" && authorityString(goals[0], "status") != "active") {
		return nil, errors.New("repository delivery goal is no longer open")
	}
	// Drop the scheduler's run and bind the original persisted execution. Role
	// and credential authority above came from the maintainer's current identity.
	bound = common.ContextWithRun(bound, common.RunContext{RunId: j.RunId, GoalId: j.GoalId, OwnerUserId: j.OwnerUserId, Mode: j.Mode})
	return auth.ContextWithInternalOrigin(bound), nil
}
