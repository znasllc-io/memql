package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	"github.com/znasllc-io/memql/core/airoute"
)

func pipelineReviewDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (a *App) pipelineReviewSource(ctx context.Context, p pipelinerun.Pipeline, r pipelinerun.Run, fence func(context.Context) error) (pipelinerun.ReviewSource, *githubapp.Client, string, error) {
	empty := pipelinerun.ReviewSource{}
	if a.engine == nil || fence == nil || !pipelinerun.QualificationSourceAllowed(p, r, time.Now().UTC()) || p.PackageID == "" {
		return empty, nil, "", errors.New("pipeline review needs its active source or exact unexpired pull-request qualification")
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
	source   func(context.Context, pipelinerun.Pipeline, pipelinerun.Run, func(context.Context) error) (pipelinerun.ReviewSource, *githubapp.Client, string, error)
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
	// An inactive pipeline carries no general review authority. Qualification is
	// a persisted grant for this exact run; the source adapter and its fresh
	// driver fence still recheck provider ownership and current grant afterward.
	if !pipelinerun.QualificationSourceAllowed(p, r, time.Now().UTC()) {
		return input, errors.New("independent review source has no authority for this run")
	}
	source, gh, reader, err := reads.source(ctx, p, r, fence)
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
		// Ordinary review can prove a disjoint target advance below. A bounded
		// qualification instead binds the previewed base exactly; retargeting or
		// base movement requires a fresh owner request.
		if r.Qualification != nil && pr.BaseSHA != r.BaseSHA {
			return input, errors.New("qualified review base no longer matches its approved run")
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
	current, _, _, err := reads.source(ctx, p, r, fence)
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
	source, gh, reader, err := a.pipelineReviewSource(ctx, p, r, fence)
	if err != nil {
		return err
	}
	if source != input.Source {
		return errors.New("independent review source changed before semantic validation")
	}
	if err = samePipelineIndependentReview(ctx, input, fresh, gh, reader); err != nil {
		return err
	}
	current, _, _, err := a.pipelineReviewSource(ctx, p, r, fence)
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
	ctx = pipelineCallContext(ctx)
	const schema = `{"type":"object","additionalProperties":false,"required":["approved","reason"],"properties":{"approved":{"type":"boolean"},"reason":{"type":"string","minLength":1,"maxLength":4000}}}`
	var result memql.StructuredAIResult
	if input.Projection != nil {
		result, err = a.engine.InvokeCapturedPromptStructuredBounded(ctx, input.Prompt, data, "pipeline_independent_review", schema, true, pipelineReviewRenderedLimit)
	} else {
		result, err = a.engine.InvokeCapturedPromptStructured(ctx, input.Prompt, data, "pipeline_independent_review", schema, true)
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(result.Text), &out)
	return out, err
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

// pipelineCallContext marks a model call the pipeline runner itself makes as
// pipeline work, so the shipped rule pipelineLocalFirst routes it to the
// owner's machines first, then an app, then a vendor (spec 2026-10-29, D5).
// The review renders a captured prompt and builds no request of its own, so
// the tag rides the call attribution; whatever an outer frame attributed is
// kept.
func pipelineCallContext(ctx context.Context) context.Context {
	a, _ := memql.AICallAttributionFromContext(ctx)
	if !slices.Contains(a.Tags, airoute.TagPipeline) {
		a.Tags = append(append([]string(nil), a.Tags...), airoute.TagPipeline)
	}
	return memql.ContextWithAICallAttribution(ctx, a)
}
