package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
)

// trigger.go -- a staged GitHub delivery becomes runs (design record D4-D6).
//
// THE TRIGGER IS HANDED A ROW ID AND NOTHING ELSE. pipelinesTrigger is a
// builtin, and a builtin is callable by any signed-in client's query (@sdk has
// no engine effect), so two things stand between a caller and a forged run:
// the trigger refuses every call that did not arrive with internal origin --
// which the automation executor stamps on a tree-loaded automation's step
// context and on nothing a client sends -- before it reads anything; and it
// reads the delivery it acts on from the STAGED v1:platform:inboundRequest
// row, requiring the github source and the receiver's signatureVerified, so
// the body that decides what opens is the one GitHub signed and never one an
// argument supplied.
//
// The event is read from the SIGNED body's shape (decision 4,
// pipelines.ClassifyDelivery); X-GitHub-Event is a cross-check only, and the
// delivery id is correlation only. Neither is proof of anything: the body's
// HMAC does not sign them.

// githubSource is the inbound source segment GitHub delivers on.
const githubSource = "github"

// SkippedPipeline is a pipeline a delivery or a poll did not act on, and why.
type SkippedPipeline struct {
	PipelineID string `json:"pipelineId"`
	Reason     string `json:"reason"`
}

// TriggerResult is what one delivery did.
type TriggerResult struct {
	// Source is the staged row's source segment.
	Source string
	// Ignored is why the delivery asks pipelines for nothing; empty when it
	// asked for something.
	Ignored    string
	Event      pipelines.Event
	Rerequest  bool
	Repository string
	SHA        string
	// Opened are the runs this delivery opened; Existing the runs that
	// already answered it (a redelivery, or a poll that saw the head first).
	Opened   []Run
	Existing []Run
	Skipped  []SkippedPipeline
}

func (i *Integration) handleTrigger(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	inboundRequestID := stringArg(args, "inboundRequestId")
	res, err := i.Trigger(handlerContext(ctx), inboundRequestID)
	if err != nil {
		return nil, err
	}
	answer := map[string]any{
		// The staged row this answer is about, for an operator following a
		// delivery from the inbound seam to its runs.
		"inboundRequestId": inboundRequestID,
		"source":           res.Source,
		"repository":       res.Repository,
		"event":            string(res.Event),
		"rerequest":        res.Rerequest,
		"sha":              res.SHA,
		"opened":           runIDs(res.Opened),
		"existing":         runIDs(res.Existing),
		"skipped":          res.Skipped,
	}
	if res.Ignored != "" {
		answer["ignored"] = res.Ignored
	}
	return resultNode(answer), nil
}

// Trigger opens the runs the delivery staged as inboundRequestID asks for, on
// behalf of the shipped automation that fires on its row. A call that did not
// arrive with internal origin is refused (ErrClientOrigin) before anything is
// read; a row staged on another source is Ignored -- not an error: most of
// what reaches the inbound seam is about something else; a github row the
// receiver did not verify is refused (ErrDeliveryUnverified); and a delivery
// pipelines WOULD act on that cannot be read is an error.
func (i *Integration) Trigger(ctx context.Context, inboundRequestID string) (TriggerResult, error) {
	if err := requireInternalOrigin(ctx, "trigger"); err != nil {
		return TriggerResult{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return TriggerResult{}, errNoStore
	}
	if strings.TrimSpace(inboundRequestID) == "" {
		return TriggerResult{}, fmt.Errorf("pipelines: inboundRequestId is required")
	}
	staged, err := d.Store.InboundDelivery(ctx, inboundRequestID)
	if err != nil {
		return TriggerResult{}, err
	}
	if staged == nil {
		return TriggerResult{}, fmt.Errorf("pipelines: no delivery is staged as %q", inboundRequestID)
	}
	res := TriggerResult{Source: staged.Source}
	if !strings.EqualFold(strings.TrimSpace(staged.Source), githubSource) {
		res.Ignored = fmt.Sprintf("source %q is not GitHub", staged.Source)
		return res, nil
	}
	if !staged.SignatureVerified {
		return res, fmt.Errorf("%w (staged as %q)", ErrDeliveryUnverified, inboundRequestID)
	}

	headerEvent, deliveryID := deliveryHeaders(staged.HeadersJSON)
	t, ignored, err := pipelines.ClassifyDelivery(headerEvent, []byte(staged.Body))
	if err != nil {
		return res, err
	}
	if ignored != nil {
		res.Ignored = ignored.Reason
		return res, nil
	}
	res.Event, res.Rerequest, res.Repository, res.SHA = t.Event, t.Rerequest, t.Repository, t.SHA

	matched, err := d.Store.PipelinesForRepository(ctx, t.Repository)
	if err != nil {
		return res, err
	}
	var failures []error
	for _, p := range matched {
		if !p.Active() {
			res.Skipped = append(res.Skipped, SkippedPipeline{PipelineID: p.ID, Reason: "the pipeline is disconnected"})
			continue
		}
		// The installation is part of the match: a second installation of
		// the same repository -- another app's, or this app's on a fork's
		// organization -- cannot drive a pipeline it was never connected
		// through.
		if p.InstallationID != t.InstallationID {
			res.Skipped = append(res.Skipped, SkippedPipeline{PipelineID: p.ID,
				Reason: fmt.Sprintf("delivered to installation %d; this pipeline is connected through installation %d", t.InstallationID, p.InstallationID)})
			continue
		}
		var (
			opened OpenResult
			skip   string
			oerr   error
		)
		switch {
		case t.Rerequest:
			opened, skip, oerr = i.rerequested(ctx, d, p, t, deliveryID)
		case t.Event == pipelines.EventRelease:
			opened, skip, oerr = i.released(ctx, d, p, t, deliveryID)
		default:
			opened, oerr = i.open(ctx, d, p, Opening{
				Event: t.Event, SHA: t.SHA, BaseSHA: t.BaseSHA, Branch: t.Branch, Title: t.Title,
				PullRequest: t.PullRequest, Fork: t.Fork, HeadRepository: t.HeadRepository,
				Trigger: TriggerWebhook, DeliveryID: deliveryID,
			})
		}
		switch {
		case oerr != nil:
			failures = append(failures, fmt.Errorf("pipeline %s: %w", p.ID, oerr))
		case skip != "":
			res.Skipped = append(res.Skipped, SkippedPipeline{PipelineID: p.ID, Reason: skip})
		case opened.Opened:
			res.Opened = append(res.Opened, opened.Run)
		default:
			res.Existing = append(res.Existing, opened.Run)
		}
	}
	return res, errors.Join(failures...)
}

// rerequested re-runs what a re-requested check run or check suite names: the
// run reporting that check run, or the newest run of this pipeline at the
// suite's SHA -- as the next attempt, in the original's mode and event
// (decision 3).
func (i *Integration) rerequested(ctx context.Context, d Deps, p Pipeline, t pipelines.Trigger, deliveryID string) (OpenResult, string, error) {
	var original *Run
	if t.CheckRunID > 0 {
		r, err := d.Store.RunByCheckRun(ctx, t.Repository, t.CheckRunID)
		if err != nil {
			return OpenResult{}, "", err
		}
		if r == nil {
			return OpenResult{}, fmt.Sprintf("no run reports check run %d", t.CheckRunID), nil
		}
		if !sameID(r.PipelineID, p.ID) {
			return OpenResult{}, "the re-requested check run reports another pipeline's run", nil
		}
		original = r
	} else {
		runs, err := d.Store.RunsForPipelineSHA(ctx, p.ID, t.SHA)
		if err != nil {
			return OpenResult{}, "", err
		}
		if len(runs) == 0 {
			return OpenResult{}, "this pipeline has no run at the re-requested commit", nil
		}
		newest := runs[0]
		for _, r := range runs[1:] {
			if r.QueuedAt.After(newest.QueuedAt) {
				newest = r
			}
		}
		original = &newest
	}
	if original.RefusalCode == pipelines.CodeForkRefused {
		return OpenResult{}, "a pull request from a fork is never run, re-requested or not", nil
	}
	// GitHub's re-request re-runs the whole check: one check run reports
	// the whole run, so there is no failed half of it to ask for.
	opened, err := i.open(ctx, d, p, rerunOf(*original, deliveryID, false))
	if errors.Is(err, ErrRunInProgress) {
		return opened, "the commit's newest run has not finished", nil
	}
	return opened, "", err
}

// released opens a release's run once its tag resolves: the tag's commit,
// asked of GitHub under the owner's grant rather than trusted from a branch
// name.
func (i *Integration) released(ctx context.Context, d Deps, p Pipeline, t pipelines.Trigger, deliveryID string) (OpenResult, string, error) {
	if d.GitHub == nil {
		return OpenResult{}, "", errNoGitHub
	}
	token, _, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return OpenResult{}, "", err
	}
	sha, _, err := d.GitHub.CommitForRef(ctx, token, p.Repository, "tags/"+t.ReleaseTag)
	if err != nil {
		return OpenResult{}, "", fmt.Errorf("release %s: resolving its tag: %w", t.ReleaseTag, err)
	}
	opened, err := i.open(ctx, d, p, Opening{
		Event: pipelines.EventRelease, SHA: sha, Title: t.Title, ReleaseTag: t.ReleaseTag,
		Trigger: TriggerWebhook, DeliveryID: deliveryID,
	})
	return opened, "", err
}

// rerunOf is the opening of r's next attempt: whole, or -- failedOnly, a
// person's "Re-run failed" -- running only what r did not pass.
func rerunOf(r Run, deliveryID string, failedOnly bool) Opening {
	return Opening{
		Event: r.Event, Mode: r.Mode, SHA: r.SHA, BaseSHA: r.BaseSHA, Branch: r.HeadBranch,
		Title: r.Title, PullRequest: r.PullRequest, Version: r.Version,
		Trigger: TriggerRerun, RerunOf: r.ID, DeliveryID: deliveryID, FailedOnly: failedOnly,
	}
}

// deliveryHeaders reads the event and delivery id out of the staged headers
// (component/inbound stages them lower-cased). An unreadable object is no
// headers: the body decides what a delivery is, and the header is only a
// cross-check.
func deliveryHeaders(headersJSON string) (event, deliveryID string) {
	if strings.TrimSpace(headersJSON) == "" {
		return "", ""
	}
	var headers map[string]any
	if json.Unmarshal([]byte(headersJSON), &headers) != nil {
		return "", ""
	}
	for name, value := range headers {
		s, _ := value.(string)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "x-github-event":
			event = strings.TrimSpace(s)
		case "x-github-delivery":
			deliveryID = strings.TrimSpace(s)
		}
	}
	return event, deliveryID
}

func runIDs(runs []Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}
