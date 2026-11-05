package campaigns

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// compose_run.go -- one email composition run: the one step of the
// composeEmailRun template (dsl/campaigns/automations.memql), as the builtin
// campaignRunComposition.
//
// campaignComposeEmail opens a goal for a template whose composition is
// writing, on route:apps at level strong, and the goal's run comes here on the
// agent replica that claimed it, under the owner's own authority. The run:
//
//  1. claims the composition for its goal, under the template's lock (below);
//  2. reads the material (compositionMaterial) and files EVERY file of it in the
//     owner's Library, folder "Email material", under its workspace name, plain
//     files as well as ZIP members, so the cockpit pulls each into the
//     session's workspace under the name the context gives it (one Library
//     file per name: two files with one name would land on one path);
//  3. calls the engine's bounded tool loop with the emailCompose prompt and
//     exactly the three emailCompose tools, acting as the owner's email
//     composer (the per-user seed agent emailComposerAgent, a specialist), with
//     templateId and goalId stamped on every tool and the filed files as the
//     session's inputs. On the Apps route that call is ONE Claude Code session
//     on the owner's machine, which takes the step whole and reaches exactly
//     those tools over MCP;
//  4. when the call returns, archives the files it filed -- those only: the
//     person's own attachments are theirs -- and, when the composition is
//     still writing for this goal (nothing was submitted), fails it with the
//     reason.
//
// # Who claims the composition
//
// campaignComposeEmail records the goal on the composition once the goal is
// open, and the goal's run may start before that write lands. So the run
// claims the composition itself, under the template's lock: a writing
// composition with no goal gets this run's goal (compare and set), one with
// this goal is this run's, and one with ANOTHER goal belongs to a later start
// (a retry) and this run is stale: it leaves without touching anything. A
// composition that is not writing is left as it is.
//
// # Apps only
//
// A composition runs on the Apps route and nowhere else. A run whose route is
// not route:apps, or whose step override pins a model that is not an app, is
// refused before anything is touched; a route never falls through to another,
// so nothing reaches a local model.
//
// # How a run ends
//
// The composition is the record a person reads, so it is what a run settles
// last. When the run fails it, the run's error carries composition_failed, the
// work spine's code for a step whose work recorded its own failure
// (component/work's TerminalCompositionFailed): the run fails instead of being
// retried, since a retry would find the composition failed and do nothing. A
// cancellation is not that: the composition records it, and the run answers the
// cancellation. The archive and the last write use a context the step's
// cancellation does not reach.
//
// # What a person reads
//
// Why a composition failed is shown on the template, on the OS list and as the
// editor's bar, so it is one of a few fixed sentences of at most ten words
// (compositionReason), as every sentence of a start is (compose_start.go):
// "Cancelled"; "Your Claude Code app could not be reached." for an app door
// nobody can reach or a route that cannot serve the person; the package's own
// short sentences (busy, nothing saved, the material's); and otherwise "MemQL
// could not finish this email." What is longer than a sentence -- which files
// were left out, and why -- is the composition's notes. The cause itself is
// logged with the template, and never shown.

const (
	// emailComposePrompt is the prompt the composer is handed (dsl/campaigns/prompts.memql).
	emailComposePrompt = "emailCompose"

	// emailComposerAgent names the per-user seed agent a run acts as
	// (dsl/agents/emailComposerAgent.memql): the materializer gives each user
	// one, with the id "<seed>-<user>", and the tools require its role.
	emailComposerAgent = "emailComposerAgent"
	emailComposerRole  = "specialist"

	// materialFolder is the owner's Library folder a run files material in.
	materialFolder = "Email material"

	// compositionFailedCode is the stable word the work spine reads, in a
	// run's error, as "the composition recorded its own failure"
	// (component/work.TerminalCompositionFailed).
	compositionFailedCode = "composition_failed"

	// compositionCancelled is the reason a cancelled composition records.
	compositionCancelled = "Cancelled"

	// reasonAppUnreachable is the reason a composition records when no app
	// door of the owner's could be reached, or the route could not serve them:
	// the app is named as the start's refusals name it (errComposeNeedsApp).
	reasonAppUnreachable = "Your Claude Code app could not be reached."

	// reasonCouldNotFinish is the reason for any other cause.
	reasonCouldNotFinish = "MemQL could not finish this email."

	// maxComposerSummaryRunes bounds the composer's closing words in the run's answer.
	maxComposerSummaryRunes = 4000

	// compositionFinalWait bounds the archive and the last write of a run.
	compositionFinalWait = 30 * time.Second
)

// emailComposeTools are the tools a composition is given: the three of
// dsl/campaigns/tools.memql, and nothing else.
var emailComposeTools = []string{"emailComposeContext", "emailComposeRender", "emailComposeSubmit"}

var (
	errNothingSaved     = errors.New("MemQL finished without saving the email.")
	errRunNeedsGoal     = errors.New("An email composition runs only as its own goal's run.")
	errRunNeedsApps     = errors.New("An email is composed on the Apps route only.")
	errNoMaterialFiler  = errors.New("This node cannot hand a composition its files: it has no Library storage wired.")
	errNoComposerLoop   = errors.New("This node cannot run a composition: it has no model tool loop.")
	errCompositionMoved = errors.New("The template this composition writes is no longer in its organization.")
)

// MaterialFiler files the material of an email composition in its owner's
// Library, so the composer's session can be handed it, and archives it after.
// The application installs one where object storage is resolved
// (SetMaterialFiler).
type MaterialFiler interface {
	// File stores data as a new Library file of ownerUserID, named name, in
	// the owner's folder named folder (made when missing), and answers its
	// file and the artifact a session's inputs name. When it fails after the
	// file was written it still answers the file, so that it can be archived.
	File(ctx context.Context, ownerUserID, folder, name, mime string, data []byte) (FiledMaterial, error)
	// Archive archives a file File filed: its artifact, when it has one, and
	// the file.
	Archive(ctx context.Context, ownerUserID string, f FiledMaterial) error
}

// FiledMaterial is one material file in its owner's Library: the file, and the
// Library artifact a session's inputs carry. ArtifactID is empty when filing
// failed before the Library listed the file.
type FiledMaterial struct{ FileID, ArtifactID string }

// SetMaterialFiler installs the Library writer a composition run files its
// material with.
func (w *Worker) SetMaterialFiler(f MaterialFiler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.materialFiler = f
}

func (w *Worker) filer() MaterialFiler {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.materialFiler
}

// toolLoop is the engine's bounded tool loop, the one way a run calls a model.
// It is resolved off the engine rather than added to Engine, so the campaigns
// surface gains no model call anywhere else (integrations/knowledge's
// trainerEngine makes the same choice).
type toolLoop interface {
	InvokeAIChatWithFilteredTools(ctx context.Context, templateID string, data map[string]any, toolNames []string) (string, error)
}

// handleRunComposition implements integration.campaigns.runComposition.
func (w *Worker) handleRunComposition(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	templateID := bare(strings.TrimSpace(argString(args, "templateId")))
	if templateID == "" || !templateIDPattern.MatchString(templateID) {
		return nil, errComposeTemplateMissing
	}
	run, inRun := common.RunFromContext(ctx)
	owner := strings.TrimSpace(run.OwnerUserId)
	if !inRun || strings.TrimSpace(run.GoalId) == "" || owner == "" || bare(callerUserID(ctx)) != bare(owner) {
		return nil, errRunNeedsGoal
	}
	if err := appsOnly(run); err != nil {
		return nil, err
	}
	t, claimed, err := w.claimComposition(ctx, templateID, run.GoalId)
	if err != nil {
		return nil, err
	}
	answer := map[string]any{"templateId": templateID, "goalId": run.GoalId, "state": t.composition.State}
	if !claimed {
		answer["skipped"] = "This template has no composition writing for this run's goal; nothing was done."
		return resultNode("campaignCompositionRun", answer)
	}
	state, summary, err := w.composeRun(ctx, run, t)
	if err != nil {
		return nil, err
	}
	answer["state"], answer["summary"] = state, summary
	return resultNode("campaignCompositionRun", answer)
}

// appsOnly refuses a run that would call a model anywhere but the Apps route.
func appsOnly(run common.RunContext) error {
	if id, isRoute := memql.RouteIdFromSource(run.Routing.Source); !isRoute || id != memql.RouteIdApps {
		return errRunNeedsApps
	}
	if ov := run.Override; ov != nil && strings.TrimSpace(ov.Model) != "" {
		if _, _, isApp := memql.SplitAppReference(ov.Model); !isApp {
			return errRunNeedsApps
		}
	}
	return nil
}

// claimComposition reads the template under its lock and claims its writing
// composition for goalID (see the header). It answers the template, and true
// when this run is to write it.
func (w *Worker) claimComposition(ctx context.Context, templateID, goalID string) (composingTemplate, bool, error) {
	if w.templateGate == nil {
		return composingTemplate{}, false, errTemplateGateUnavailable
	}
	release, err := w.lockTemplate(ctx, templateID)
	if err != nil {
		return composingTemplate{}, false, err
	}
	defer release()
	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", templateID}))
	if err != nil {
		return composingTemplate{}, false, err
	}
	if len(rows) == 0 {
		return composingTemplate{}, false, errComposeTemplateMissing
	}
	row := rows[0]
	accountID := bare(str(row, "accountId"))
	if accountID == "" {
		return composingTemplate{}, false, errComposeTemplateMissing
	}
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return composingTemplate{}, false, err
	}
	if !w.mayWriteTemplates(ctx, accountID) {
		return composingTemplate{}, false, errTemplateNeedsWrite
	}
	t := composingTemplate{id: templateID, accountID: accountID, name: str(row, "name")}
	c, ok := compositionFromRow(row)
	if !ok {
		return t, false, nil
	}
	t.composition = c
	switch {
	case c.State != compositionWriting:
		return t, false, nil
	case c.GoalID == "":
		// Not recorded yet: this run's goal is the composition's. A
		// composition with no revision to start from starts from the row as
		// it is, which nobody can have changed while it was writing.
		c.GoalID = goalID
		if c.BaseRevision.IsZero() {
			c.BaseRevision = templateTime(row["createdAt"])
		}
		if _, err := w.rewriteComposition(ctx, templateID, row, c); err != nil {
			return composingTemplate{}, false, err
		}
		t.composition = c
		return t, true, nil
	case !sameGoal(c.GoalID, goalID):
		// A later start's: this run is stale.
		return t, false, nil
	}
	return t, true, nil
}

// composeRun reads and files the material, hands the composition to the
// composer, and settles it when the composer is done. It answers the
// composition's state and the composer's closing words.
func (w *Worker) composeRun(ctx context.Context, run common.RunContext, t composingTemplate) (string, string, error) {
	owner := strings.TrimSpace(run.OwnerUserId)
	final := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), compositionFinalWait)
	}
	var filed []FiledMaterial
	archived := false
	archive := func() {
		if archived || len(filed) == 0 {
			return
		}
		archived = true
		finalCtx, cancel := final()
		defer cancel()
		w.archiveMaterial(finalCtx, owner, filed)
	}
	// A panic in the tool loop must not leave the files in the person's Library.
	defer archive()
	end := func(cause error, notes []string) (string, string, error) {
		archive()
		finalCtx, cancel := final()
		defer cancel()
		state, err := w.endComposition(finalCtx, t, run.GoalId, cause, notes)
		return state, "", err
	}

	m, err := w.compositionMaterial(ctx, t.composition.Request)
	if err != nil {
		return end(err, nil)
	}
	notes := m.notes()
	loop, ok := w.store.engine.(toolLoop)
	if !ok {
		return end(errNoComposerLoop, notes)
	}
	filer := w.filer()
	if filer == nil && len(m.Files) > 0 {
		return end(errNoMaterialFiler, notes)
	}
	inputs := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		got, err := filer.File(ctx, owner, materialFolder, f.Workspace, f.MIME, f.Data)
		if strings.TrimSpace(got.FileID) != "" {
			filed = append(filed, got)
		}
		if err == nil && strings.TrimSpace(got.ArtifactID) == "" {
			err = errors.New("the Library did not list it")
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return end(ctxErr, notes)
			}
			return end(fmt.Errorf("MemQL could not put %s in its workspace: %w", f.Workspace, err), notes)
		}
		inputs = append(inputs, got.ArtifactID)
	}
	contextAnswer, err := w.composeContext(ctx, t, m)
	if err != nil {
		return end(err, notes)
	}
	contextJSON, err := plainJSON(contextAnswer)
	if err != nil {
		return end(err, notes)
	}

	// The composer: the owner's email composer, as a specialist, with the
	// template and the goal stamped on each of its tools, and the filed
	// files for its workspace (session_inputs.go). The stamps are the
	// handover's to carry (common.DelegatedTools.Defaults): the central
	// validator drops any value the model supplies for an @autoInjected
	// field, and these put the run's own back.
	agentID := emailComposerAgent + "-" + bare(owner)
	stamps := map[string]any{"templateId": t.id, "goalId": t.composition.GoalID}
	defaults := make(map[string]map[string]any, len(emailComposeTools))
	for _, tool := range emailComposeTools {
		defaults[tool] = maps.Clone(stamps)
	}
	callCtx := memql.WithActingAgentId(ctx, agentID)
	callCtx = memql.WithActingAgentRole(callCtx, emailComposerRole)
	callCtx = memql.ContextWithAICallAttribution(callCtx, memql.AICallAttribution{AgentId: agentID})
	callCtx = common.ContextWithDelegatedToolDefaults(callCtx, defaults)
	callCtx = memql.WithSessionInputs(callCtx, inputs)
	summary, callErr := loop.InvokeAIChatWithFilteredTools(callCtx, emailComposePrompt, map[string]any{
		"description": t.composition.Request.Description, "context": string(contextJSON),
	}, slices.Clone(emailComposeTools))
	if callErr != nil && w.logger != nil {
		w.logger.Warn("campaigns: a composition's composer ended with an error", "template", t.id, "goal", run.GoalId, "error", callErr)
	}

	state, _, err := end(callErr, notes)
	if err != nil {
		return "", "", err
	}
	return state, boundedRunes(strings.TrimSpace(summary), maxComposerSummaryRunes), nil
}

// archiveMaterial archives the files a run filed. It never fails the run: the
// composition is the record, and a file left behind is logged loudly enough to
// find.
func (w *Worker) archiveMaterial(ctx context.Context, owner string, filed []FiledMaterial) {
	filer := w.filer()
	if filer == nil {
		return
	}
	for _, f := range filed {
		if err := filer.Archive(ctx, owner, f); err != nil && w.logger != nil {
			w.logger.Warn("campaigns: a composition's material file could not be archived; it stays in the owner's Library",
				"file", f.FileID, "artifact", f.ArtifactID, "error", err)
		}
	}
}

// endComposition settles a run's composition once its composer is done (or
// could not start): a composition still writing for goalID is failed, with the
// fixed reason compositionReason gives cause (when cause is nil, because
// nothing was saved) and the notes it adds; the cause itself is logged.
// Anything else -- done, failed, or started again under another goal -- is its
// own record and stays. It answers the composition's state and the run's error.
func (w *Worker) endComposition(ctx context.Context, t composingTemplate, goalID string, cause error, notes []string) (string, error) {
	reason, more := compositionReason(cause)
	notes = append(slices.Clip(notes), more...)
	if cause != nil && !errors.Is(cause, context.Canceled) && w.logger != nil {
		w.logger.Warn("campaigns: a composition run ended without its email", "template", t.id, "goal", goalID, "reason", reason, "cause", cause)
	}
	failed, state, err := w.failWritingComposition(ctx, t, goalID, reason, notes)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("campaigns: a composition run could not record how it ended", "template", t.id, "goal", goalID, "reason", reason, "error", err)
		}
		if cause != nil {
			return state, cause
		}
		return state, err
	}
	switch {
	case errors.Is(cause, context.Canceled):
		// A cancellation is the spine's own state, not a failure of the work.
		return state, cause
	case failed:
		return state, fmt.Errorf("%s: %s", compositionFailedCode, reason)
	}
	return state, nil
}

// failWritingComposition fails the template's composition with reason when it
// is still writing for goalID, in one row version under the template's lock.
// It answers whether it did, and the composition's state as it leaves it.
func (w *Worker) failWritingComposition(ctx context.Context, t composingTemplate, goalID, reason string, notes []string) (bool, string, error) {
	release, err := w.lockTemplate(ctx, t.id)
	if err != nil {
		return false, "", err
	}
	defer release()
	rows, err := w.store.rows(memql.ContextWithFreshRead(ctx), call("query", "templateById", arg{"templateId", t.id}))
	if err != nil {
		return false, "", err
	}
	if len(rows) == 0 {
		return false, "", errComposeTemplateMissing
	}
	row := rows[0]
	c, ok := compositionFromRow(row)
	if !ok {
		return false, "", errNotComposing
	}
	if c.State != compositionWriting || !sameGoal(c.GoalID, goalID) {
		return false, c.State, nil
	}
	if !sameOrganization(t.accountID, str(row, "accountId")) {
		return false, c.State, errCompositionMoved
	}
	if !w.mayWriteTemplates(ctx, t.accountID) {
		return false, c.State, errTemplateNeedsWrite
	}
	c.State, c.Error, c.FinishedAt = compositionFailed, reason, w.nowUTC()
	if len(notes) > 0 {
		c.Notes = notes
	}
	if _, err := w.rewriteComposition(ctx, t.id, row, c); err != nil {
		return false, compositionWriting, err
	}
	return true, compositionFailed, nil
}

// compositionReason is the fixed sentence a failed composition records for
// err, and the notes the failure adds (see the header). nil is a run whose
// composer saved nothing.
func compositionReason(err error) (string, []string) {
	var (
		unusable   *unusableMaterialError
		unreadable *unreadableMaterialError
	)
	switch {
	case err == nil:
		return errNothingSaved.Error(), nil
	case errors.Is(err, context.Canceled):
		return compositionCancelled, nil
	case appUnreachable(err):
		return reasonAppUnreachable, nil
	case errors.As(err, &unusable):
		return errNoUsableMaterial.Error(), slices.Clone(unusable.notes)
	case errors.As(err, &unreadable):
		return errMaterialUnreadable.Error(), []string{unreadable.Error()}
	}
	// The package's own sentences, short already, say what happened.
	for _, own := range []error{errComposeBusy, errNothingSaved} {
		if errors.Is(err, own) {
			return own.Error(), nil
		}
	}
	return reasonCouldNotFinish, nil
}

// appUnreachable reports whether err is an app door nobody could reach or a
// route that could not serve the person: the engine's refusals, as values or,
// once they have crossed a wire, as their words, which lead with their code.
func appUnreachable(err error) bool {
	var (
		unavailable *memql.AppUnavailable
		route       *memql.RouteChoiceError
	)
	if errors.As(err, &unavailable) || errors.As(err, &route) || errors.Is(err, memql.ErrAppUnavailable) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, memql.AppRefusalCode) || strings.Contains(text, memql.RouteNotReady)
}

// boundedRunes is text cut to at most n runes.
func boundedRunes(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return strings.TrimRightFunc(string([]rune(text)[:n]), unicode.IsSpace) + "..."
}
