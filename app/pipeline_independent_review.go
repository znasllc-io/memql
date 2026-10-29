package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

func pipelineReviewDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (a *App) pipelineReviewSource(ctx context.Context, p pipelinerun.Pipeline, fence func(context.Context) error) (pipelinerun.ReviewSource, *githubapp.Client, string, error) {
	empty := pipelinerun.ReviewSource{}
	if a.engine == nil || fence == nil || !p.Active() || p.PackageID == "" {
		return empty, nil, "", errors.New("pipeline review needs its active current source")
	}
	if err := fence(ctx); err != nil {
		return empty, nil, "", err
	}
	pkgs := a.lookupPackagesIntegration()
	if pkgs == nil || pkgs.GitHub() == nil {
		return empty, nil, "", errors.New("pipeline review provider is unavailable")
	}
	source, err := a.repositoryAuthoritySource(ctx, p.PackageID, true)
	if err != nil {
		return empty, nil, "", err
	}
	if source.ID != p.PackageID || source.OwnerUserID != memql.BareShortId(p.OwnerUserID) || source.CredentialID != p.CredentialID || !strings.EqualFold(source.Repository, p.Repository) {
		return empty, nil, "", errors.New("pipeline review source binding changed")
	}
	parts := strings.Split(p.Repository, "/")
	if len(parts) != 2 {
		return empty, nil, "", errors.New("pipeline review repository is invalid")
	}
	access, err := pkgs.RepositoryAccess(ctx, p.CredentialID, source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return empty, nil, "", err
	}
	if !access.CanWrite || access.InstallationID != p.InstallationID || access.RepositoryID <= 0 || access.Repository != p.Repository {
		return empty, nil, "", errors.New("pipeline review owner grant or installation changed")
	}
	reader, installation, err := pkgs.RepositoryQueueObservationToken(ctx, p.CredentialID, source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return empty, nil, "", err
	}
	if installation != p.InstallationID {
		return empty, nil, "", errors.New("pipeline review observation installation changed")
	}
	// This mints only checks:write, never content/PR/queue mutation authority. The
	// token is not retained in evidence; every actual report refreshes it again.
	_, appID, err := pkgs.RepositoryReviewCheckToken(ctx, p.CredentialID, source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return empty, nil, "", err
	}
	if appID <= 0 {
		return empty, nil, "", errors.New("pipeline review App identity is unavailable")
	}
	repo, err := pkgs.GitHub().Repository(ctx, reader, parts[0], parts[1])
	if err != nil {
		return empty, nil, "", err
	}
	if repo.ID != access.RepositoryID || !strings.EqualFold(repo.FullName, p.Repository) || repo.DefaultBranch != p.DefaultBranch {
		return empty, nil, "", errors.New("pipeline review provider repository changed")
	}
	if err = fence(ctx); err != nil {
		return empty, nil, "", err
	}
	fresh, err := a.repositoryAuthoritySource(ctx, p.PackageID, true)
	if err != nil || fresh != source {
		return empty, nil, "", errors.New("pipeline review source changed during authority verification")
	}
	return pipelinerun.ReviewSource{OwnerUserID: source.OwnerUserID, PipelineID: p.ID, SourceID: p.PackageID, CredentialID: p.CredentialID, Repository: p.Repository, Branch: p.DefaultBranch, InstallationID: p.InstallationID, RepositoryID: repo.ID, AppID: appID}, pkgs.GitHub(), reader, nil
}
func (a *App) nativePipelineReview(ctx context.Context, source pipelinerun.ReviewSource, member githubapp.MergeGroupMember) (repositoryauthority.NativeReviewMember, bool, error) {
	authority, ok := a.engine.IntegrationByName("repositoryauthority").(*repositoryauthority.Integration)
	if !ok || authority == nil {
		return repositoryauthority.NativeReviewMember{}, true, errors.New("native repository review classification is unavailable")
	}
	return authority.InspectNativeReviewMember(ctx, repositoryauthority.Source{ID: source.SourceID, OwnerUserID: source.OwnerUserID, CredentialID: source.CredentialID, Repository: source.Repository}, source.RepositoryID, member)
}

// pipelineReviewReader consists solely of native, read-only adapters. It cannot
// invoke a model, report a check, or grant repository mutation authority.
type pipelineReviewReader struct {
	source   func(context.Context, pipelinerun.Pipeline, func(context.Context) error) (pipelinerun.ReviewSource, *githubapp.Client, string, error)
	prompt   func(string) (map[string]any, error)
	native   func(context.Context, pipelinerun.ReviewSource, githubapp.MergeGroupMember) (repositoryauthority.NativeReviewMember, bool, error)
	ordinary func(context.Context, pipelinerun.Pipeline, int, string) (pipelinerun.Run, pipelinerun.IndependentReviewRecord, error)
}

func (a *App) capturePipelineIndependentReview(ctx context.Context, p pipelinerun.Pipeline, r pipelinerun.Run, promptName, policyVersion string, fence func(context.Context) error) (pipelinerun.IndependentReviewInput, error) {
	return capturePipelineIndependentReview(ctx, p, r, promptName, policyVersion, fence, pipelineReviewReader{
		source: a.pipelineReviewSource, prompt: a.engine.CapturePrompt, native: a.nativePipelineReview,
		ordinary: func(ctx context.Context, p pipelinerun.Pipeline, number int, head string) (pipelinerun.Run, pipelinerun.IndependentReviewRecord, error) {
			integration := a.lookupPipelinesIntegration()
			if integration == nil {
				return pipelinerun.Run{}, pipelinerun.IndependentReviewRecord{}, errors.New("ordinary member review store is unavailable")
			}
			return integration.ReviewForHead(ctx, p, number, head)
		},
	})
}
func capturePipelineIndependentReview(ctx context.Context, p pipelinerun.Pipeline, r pipelinerun.Run, promptName, policyVersion string, fence func(context.Context) error, reads pipelineReviewReader) (pipelinerun.IndependentReviewInput, error) {
	input := pipelinerun.IndependentReviewInput{PolicyVersion: policyVersion, PromptName: promptName}
	source, gh, reader, err := reads.source(ctx, p, fence)
	if err != nil {
		return input, err
	}
	input.Source = source
	if r.PipelineID != p.ID || memql.BareShortId(r.OwnerUserID) != memql.BareShortId(p.OwnerUserID) || r.Repository != p.Repository {
		return input, errors.New("independent review run is foreign to its source")
	}
	prompt, err := reads.prompt(promptName)
	if err != nil {
		return input, err
	}
	input.Prompt = prompt
	parts := strings.Split(source.Repository, "/")
	if r.Event == pipelines.EventPullRequest {
		pr, diff, e := gh.PullRequestReviewDiff(ctx, reader, parts[0], parts[1], r.PullRequest, r.SHA, p.DefaultBranch)
		if e != nil {
			return input, e
		}
		if pr.HeadRef != r.HeadBranch {
			return input, errors.New("review PR branch no longer matches its claimed run")
		}
		input.PullRequest = &pr
		input.Diff = &diff
		input.Kind = "ordinary"
		proof, present, e := reads.native(ctx, source, githubapp.MergeGroupMember{Number: pr.Number, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef})
		if e != nil {
			return input, e
		}
		if present {
			reviewed, e := gh.ImmutableReviewDiff(ctx, reader, parts[0], parts[1], proof.BaseSHA, pr.HeadSHA)
			if e != nil {
				return input, e
			}
			if !githubapp.SameReviewChanges(reviewed, diff) {
				return input, errors.New("native proposal changes no longer equal the complete PR diff")
			}
			rr := proof.Review
			if rr.AppID != source.AppID {
				return input, errors.New("native PR review belongs to another App")
			}
			if e = gh.VerifyReviewCheck(ctx, reader, parts[0], parts[1], rr.CheckRunID, pr.HeadSHA, pipelines.IndependentReviewCheckName, rr.ExternalID, rr.AppID); e != nil {
				return input, e
			}
			input.Kind = "native"
			input.Members = []pipelinerun.ReviewMember{{Kind: "native", RequestID: rr.RequestID, ReviewDigest: rr.ReviewDigest, ExternalID: rr.ExternalID, DiffDigest: reviewed.Digest(), CheckRunID: rr.CheckRunID, AppID: rr.AppID}}
		} else if strings.HasPrefix(pr.HeadRef, "memql/change-") {
			return input, errors.New("native proposal branch lacks its authoritative review journal; ordinary fallback is refused")
		}
	} else if r.Event == pipelines.EventMergeGroup && r.Mode == pipelines.ModeFull {
		group, e := gh.ObserveMergeGroup(ctx, reader, parts[0], parts[1], r.SHA, r.BaseSHA, r.HeadBranch)
		if e != nil {
			return input, e
		}
		if group.RepositoryID != source.RepositoryID || group.BaseBranch != source.Branch {
			return input, errors.New("review group source changed")
		}
		input.Kind = "group"
		input.Group = &group
		for _, member := range group.Members {
			proof, present, e := reads.native(ctx, source, member)
			if e != nil {
				return input, e
			}
			var reviewed githubapp.ReviewDiff
			var selected pipelinerun.ReviewMember
			if present {
				reviewed, e = gh.ImmutableReviewDiff(ctx, reader, parts[0], parts[1], proof.BaseSHA, member.HeadSHA)
				if e != nil {
					return input, e
				}
				rr := proof.Review
				selected = pipelinerun.ReviewMember{Kind: "native", RequestID: rr.RequestID, ReviewDigest: rr.ReviewDigest, ExternalID: rr.ExternalID, CheckRunID: rr.CheckRunID, AppID: rr.AppID, DiffDigest: reviewed.Digest()}
			} else {
				memberRun, saved, e := reads.ordinary(ctx, p, member.Number, member.HeadSHA)
				if e != nil {
					return input, e
				}
				if saved.Input.Source != source || saved.Input.PolicyVersion != policyVersion || saved.Input.PromptName != promptName || !reflect.DeepEqual(saved.Input.Prompt, prompt) || saved.Input.Diff == nil {
					return input, errors.New("ordinary member review source or policy was revoked")
				}
				// Reconstruct this ordinary member from current source/provider evidence.
				// The read cannot invoke the model or report another check.
				fresh, e := capturePipelineIndependentReview(ctx, p, memberRun, promptName, policyVersion, fence, reads)
				if e != nil {
					return input, e
				}
				if fresh.Kind != "ordinary" {
					return input, errors.New("ordinary member's exact source review changed")
				}
				if e = samePipelineIndependentReview(ctx, saved.Input, fresh, gh, reader); e != nil {
					return input, e
				}
				reviewed = *saved.Input.Diff
				rr := saved.Receipt
				selected = pipelinerun.ReviewMember{Kind: "ordinary", RunID: memberRun.ID, ReviewDigest: saved.InputDigest, ExternalID: rr.ExternalID, CheckRunID: rr.CheckRunID, AppID: rr.AppID, DiffDigest: reviewed.Digest()}
			}
			merged, e := gh.ImmutableReviewDiff(ctx, reader, parts[0], parts[1], member.BaseSHA, member.MergeSHA)
			if e != nil {
				return input, e
			}
			if !githubapp.SameReviewChanges(reviewed, merged) {
				return input, errors.New("synthetic member changes differ from its independently reviewed source")
			}
			if selected.AppID != source.AppID {
				return input, errors.New("merge-group member review belongs to another App")
			}
			if e = gh.VerifyReviewCheck(ctx, reader, parts[0], parts[1], selected.CheckRunID, member.HeadSHA, pipelines.IndependentReviewCheckName, selected.ExternalID, selected.AppID); e != nil {
				return input, e
			}
			input.Members = append(input.Members, selected)
		}
	} else {
		return input, errors.New("independent review is only defined for claimed PR and full merge-group runs")
	}
	// Format selection is explicit in the captured prompt, never a product name
	// or provider patch. Legacy unmarked prompts continue receiving full bytes.
	if input.Kind == "ordinary" && pipelineReviewUsesProjection(input.Prompt) {
		projection, e := githubapp.ProjectReviewDiff(ctx, *input.Diff)
		if e != nil {
			return input, e
		}
		input.Projection = &projection
		input.ProjectionDigest = projection.Digest()
		if _, e = pipelineReviewModelData(ctx, input); e != nil {
			return input, e
		}
	}
	if err = fence(ctx); err != nil {
		return input, err
	}
	current, _, _, err := reads.source(ctx, p, fence)
	if err != nil {
		return input, err
	}
	if current != source {
		return input, errors.New("independent review authority changed while reading evidence")
	}
	return input, nil
}
func (a *App) validatePipelineIndependentReview(ctx context.Context, p pipelinerun.Pipeline, r pipelinerun.Run, input pipelinerun.IndependentReviewInput, fence func(context.Context) error) error {
	fresh, err := a.capturePipelineIndependentReview(ctx, p, r, input.PromptName, input.PolicyVersion, fence)
	if err != nil {
		return err
	}
	source, gh, reader, err := a.pipelineReviewSource(ctx, p, fence)
	if err != nil {
		return err
	}
	if source != input.Source {
		return errors.New("independent review source changed before semantic validation")
	}
	if err = samePipelineIndependentReview(ctx, input, fresh, gh, reader); err != nil {
		return err
	}
	current, _, _, err := a.pipelineReviewSource(ctx, p, fence)
	if err != nil {
		return err
	}
	if current != source {
		return errors.New("independent review authority changed during semantic validation")
	}
	return nil
}

// The stored input remains immutable. Only the mutable target tip may advance,
// and only when its ancestry and complete disjoint delta are proved. Identity,
// head, merge base, full reviewed bytes, captured prompt, authority and group
// membership remain exact in both post-model validation and member reuse.
func samePipelineIndependentReview(ctx context.Context, saved, fresh pipelinerun.IndependentReviewInput, gh *githubapp.Client, reader string) error {
	if reflect.DeepEqual(saved, fresh) {
		return ctx.Err()
	}
	if (saved.Kind != "ordinary" && saved.Kind != "native") || saved.PullRequest == nil || fresh.PullRequest == nil || saved.Diff == nil || fresh.Diff == nil || gh == nil {
		return errors.New("independent review immutable source, membership or policy changed")
	}
	current := *fresh.PullRequest
	priorBase := saved.PullRequest.BaseSHA
	currentBase := current.BaseSHA
	current.BaseSHA = priorBase
	comparison := fresh
	comparison.PullRequest = &current
	if priorBase == currentBase || !reflect.DeepEqual(saved, comparison) {
		return errors.New("independent review source, head, merge base, changes or policy changed")
	}
	parts := strings.Split(saved.Source.Repository, "/")
	if len(parts) != 2 {
		return errors.New("independent review repository is invalid")
	}
	return gh.VerifyReviewTargetAdvance(ctx, reader, parts[0], parts[1], priorBase, currentBase, *saved.Diff)
}

const pipelineReviewRenderedLimit = 1 << 20

func pipelineReviewUsesProjection(prompt map[string]any) bool {
	source, _ := prompt["source"].(string)
	first, _, _ := strings.Cut(source, "\n")
	return first == githubapp.ReviewProjectionPromptMarker
}

// Native capture calls this before saving or marking ModelAttempted. Validation
// calls capture again; invocation verifies the same immutable bytes once more.
func pipelineReviewModelData(ctx context.Context, input pipelinerun.IndependentReviewInput) (map[string]any, error) {
	if input.Kind != "ordinary" || input.Diff == nil || input.PullRequest == nil {
		return nil, errors.New("independent review has no ordinary source")
	}
	var changes any = input.Diff
	if input.Projection != nil {
		if input.ProjectionDigest != input.Projection.Digest() {
			return nil, errors.New("review projection digest mismatch")
		}
		if !pipelineReviewUsesProjection(input.Prompt) {
			return nil, errors.New("review prompt does not declare the projection format")
		}
		if err := githubapp.ValidateReviewProjection(ctx, *input.Diff, *input.Projection); err != nil {
			return nil, err
		}
		changes = input.Projection
	} else if input.ProjectionDigest != "" || pipelineReviewUsesProjection(input.Prompt) {
		return nil, errors.New("review prompt requires a captured complete-hunk projection")
	}
	diff, err := json.Marshal(changes)
	if err != nil {
		return nil, err
	}
	pr, err := json.Marshal(input.PullRequest)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"repository": input.Source.Repository, "pullRequest": string(pr), "changes": string(diff)}
	if input.Projection != nil {
		rendered, e := memql.RenderCapturedPromptBounded(input.Prompt, data, pipelineReviewRenderedLimit)
		if e != nil {
			return nil, e
		}
		if !strings.HasPrefix(rendered, githubapp.ReviewProjectionPromptMarker+"\n") {
			return nil, errors.New("compiled review prompt does not declare its input format")
		}
	}
	return data, nil
}

// The driver has fenced the captured source and its current provider grant.
// Scheduled execution has no originating user session, so restore the source's
// actual local owner before model attribution instead of borrowing the scheduler
// or another incoming actor. Re-read the binding before using it as authority;
// persisted evidence alone must not revive a removed or reassigned source.
func (a *App) pipelineIndependentReviewContext(ctx context.Context, source pipelinerun.ReviewSource) (context.Context, error) {
	if a.engine == nil || auth.NamesNoPerson(source.OwnerUserID) || source.SourceID == "" {
		return nil, errors.New("independent review source owner is unavailable")
	}
	current, err := a.repositoryAuthoritySource(ctx, source.SourceID, true)
	if err != nil {
		return nil, err
	}
	if current.OwnerUserID != memql.BareShortId(source.OwnerUserID) || current.CredentialID != source.CredentialID || current.Repository != source.Repository {
		return nil, errors.New("independent review source owner or connection changed")
	}
	bound, err := a.repositoryMaintainerContext(ctx, current.OwnerUserID)
	if err != nil {
		return nil, err
	}
	// Restoring a forwarded person resets DSL origin to client. Preserve only
	// the origin this native call already held, not a new server-side grant.
	if auth.OriginFromContext(ctx).IsInternal() {
		bound = auth.ContextWithInternalOrigin(bound)
	}
	return repositoryIndependentReviewContext(bound), nil
}

func (a *App) invokePipelineIndependentReview(ctx context.Context, input pipelinerun.IndependentReviewInput) (pipelinerun.IndependentReviewVerdict, error) {
	var out pipelinerun.IndependentReviewVerdict
	if input.Kind != "ordinary" || input.Diff == nil || input.PullRequest == nil || len(input.Prompt) == 0 {
		return out, errors.New("independent model review has no exact read-only input")
	}
	data, err := pipelineReviewModelData(ctx, input)
	if err != nil {
		return out, err
	}
	ctx, err = a.pipelineIndependentReviewContext(ctx, input.Source)
	if err != nil {
		return out, err
	}
	const schema = `{"type":"object","additionalProperties":false,"required":["approved","reason"],"properties":{"approved":{"type":"boolean"},"reason":{"type":"string","minLength":1,"maxLength":4000}}}`
	var result memql.StructuredAIResult
	if input.Projection != nil {
		result, err = a.engine.InvokeCapturedPromptStructuredBounded(ctx, input.Prompt, data, "pipeline_independent_review", schema, true, pipelineReviewRenderedLimit)
	} else {
		result, err = a.engine.InvokeCapturedPromptStructured(ctx, input.Prompt, data, "pipeline_independent_review", schema, true)
	}
	if err != nil {
		return out, a.pipelineModelFailure(ctx, input, err)
	}
	err = json.Unmarshal([]byte(result.Text), &out)
	return out, err
}

// pipelineReviewModelFailure is the independent review's failed model call as
// the pipeline reports it: Error() is the router's wording for the failure (a
// stable code and its words, router.FailureReason), and Unwrap() is the original.
//
// WHY THE WORDS AND THE ORIGINAL TRAVEL TOGETHER. The workflow's error is
// printed whole on the run's check run ("Pipeline workflow failed: ..."), which
// is public on GitHub for a public repository, and the driver's mask removes
// secret values only. A model call's error is raw runtime text: a door that
// served from the owner's machine begins with that machine's label (the name its
// Cockpit reported, a hostname, unless the owner chose one), a vendor's error
// quotes what the vendor sent back, a local runtime's can carry a path or a
// request id. So the text the pipeline carries is the router's own line for a
// failed call, built from a code table and never from the error's text.
//
// Unwrap keeps the original for a caller that decides on the error's TYPE
// (errors.Is, errors.As: a run ceiling, a refusal's doors, a cancel or a
// deadline). Nothing in the pipeline's own chain branches on one today, so that
// holds for a caller added later. Only the words change. The router's refusals
// keep their code in the line (every_door_shut, ceiling_reached,
// no_local_model_available, no_app_available), which is what
// work.InferenceRefusalCode matches in a failure that travels as a string; the
// no-owner app refusal reads app_no_owner there. Covered by
// TestAFailedReviewModelCallIsReportedByItsCodeNotItsText.
type pipelineReviewModelFailure struct {
	words string
	err   error
}

func (f *pipelineReviewModelFailure) Error() string { return f.words }
func (f *pipelineReviewModelFailure) Unwrap() error { return f.err }

// pipelineModelFailure words a failed model call for the pipeline and logs the
// original for the owner.
//
// THE ORIGINAL IS NOT LOST: it goes to this node's log at warn level, tagged
// with the pipeline, repository, pull request and head SHA, so the owner who
// needs the machine's or the vendor's own words finds them in Logs, a place only
// they can see. The line takes the work run as its subject only when the context
// carries one, and the pipeline driver's does not (it drives from a background
// context through borrowed-run scoped execution), so in production the tags are
// what find it. The line also says what the pipeline reported in their place.
// It covers every failure of the prompt call, including one before any model was
// asked (a prompt that would not render), which reads by its category. Logging is best effort and never
// stands between the failure and its wording: a node with no logger still words
// the error.
//
// A consequence to know: the owner's work run record carries the worded text too,
// because that is the error the run fails with. The original lives in the log
// (and in the router's call ledger, which records the provider's own message).
func (a *App) pipelineModelFailure(ctx context.Context, input pipelinerun.IndependentReviewInput, err error) error {
	if err == nil {
		return nil
	}
	failure := &pipelineReviewModelFailure{words: router.FailureReason(err), err: err}
	if a != nil && a.Logger != nil {
		attrs := []any{
			"component", "pipelinerun",
			"pipeline", input.Source.PipelineID, "repository", input.Source.Repository,
			"reported", failure.words, "error", err.Error(),
		}
		if pr := input.PullRequest; pr != nil {
			attrs = append(attrs, "pullRequest", pr.Number, "headSha", pr.HeadSHA)
		}
		if rc, ok := common.RunFromContext(ctx); ok && rc.RunId != "" {
			attrs = append(attrs, logger.Subject(memorynodes.ConceptWorkRun, memql.BareShortId(rc.RunId)))
		}
		a.Logger.Warn("pipelines: the independent review's prompt call failed; the check run reports its code and not its text", attrs...)
	}
	return failure
}

func (a *App) reportPipelineIndependentReview(ctx context.Context, input pipelinerun.IndependentReviewInput, external string, observeOnly bool, fence func(context.Context) error) (pipelinerun.IndependentReviewReceipt, error) {
	return reportIndependentPipelineReview(ctx, a.lookupPackagesIntegration(), input, external, observeOnly, fence)
}

func reportIndependentPipelineReview(ctx context.Context, pkgs repositoryReviewAccess, input pipelinerun.IndependentReviewInput, external string, observeOnly bool, fence func(context.Context) error) (pipelinerun.IndependentReviewReceipt, error) {
	empty := pipelinerun.IndependentReviewReceipt{}
	source := input.Source
	parts := strings.Split(source.Repository, "/")
	if pkgs == nil || pkgs.GitHub() == nil || len(parts) != 2 || fence == nil {
		return empty, errors.New("independent report authority is unavailable")
	}
	if err := fence(ctx); err != nil {
		return empty, err
	}
	reader, installation, err := pkgs.RepositoryQueueObservationToken(ctx, source.CredentialID, source.OwnerUserID, parts[0], parts[1])
	if err != nil {
		return empty, err
	}
	if installation != source.InstallationID {
		return empty, errors.New("review report installation changed")
	}
	refresh := func(c context.Context) (string, error) {
		if e := fence(c); e != nil {
			return "", e
		}
		token, appID, e := pkgs.RepositoryReviewCheckToken(c, source.CredentialID, source.OwnerUserID, parts[0], parts[1])
		if e != nil {
			return "", e
		}
		if appID != source.AppID {
			return "", errors.New("review report App changed")
		}
		if e = fence(c); e != nil {
			return "", e
		}
		return token, nil
	}
	gh := pkgs.GitHub()
	head := ""
	if input.PullRequest != nil {
		head = input.PullRequest.HeadSHA
	}
	if input.Group != nil {
		head = input.Group.HeadSHA
	}
	if input.Kind == "native" {
		if len(input.Members) != 1 {
			return empty, errors.New("native review receipt is missing")
		}
		observeOnly = true
		external = input.Members[0].ExternalID
	}
	var id int64
	if observeOnly {
		id, err = gh.ObserveReviewCheck(ctx, reader, parts[0], parts[1], head, pipelines.IndependentReviewCheckName, external, source.AppID)
		if err == nil && id == 0 {
			err = errors.New("previous review report is still unconfirmed; no effect was repeated")
		}
	} else if input.Kind == "ordinary" && input.PullRequest != nil {
		id, err = gh.ReportReviewCheck(ctx, reader, parts[0], parts[1], input.PullRequest.Number, head, input.Source.Branch, pipelines.IndependentReviewCheckName, external, source.AppID, refresh)
	} else if input.Kind == "group" && input.Group != nil {
		id, err = gh.ReportMergeGroupReviewCheck(ctx, reader, parts[0], parts[1], *input.Group, pipelines.IndependentReviewCheckName, external, source.AppID, func(c context.Context, _ githubapp.MergeGroupEvidence) (string, error) { return refresh(c) })
	} else {
		err = errors.New("review report has an invalid native intent kind")
	}
	if err != nil {
		return empty, err
	}
	if err = fence(ctx); err != nil {
		return empty, err
	}
	return pipelinerun.IndependentReviewReceipt{CheckName: pipelines.IndependentReviewCheckName, HeadSHA: head, ExternalID: external, CheckRunID: id, AppID: source.AppID}, nil
}
