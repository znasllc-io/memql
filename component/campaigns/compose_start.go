package campaigns

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// compose_start.go -- starting, cancelling and retrying an email composition:
// the builtins campaignComposeEmail ("Write it", and Ask once the person has
// chosen a layout), campaignCancelComposition and campaignRetryComposition.
//
// # Starting: nothing waits
//
// campaignComposeEmail makes the template at once and answers
// campaignCompositionStarted {templateId, goalId, revision, state}; the
// composition's goal writes the email later, whole in Claude Code on the
// caller's own machine (compose_run.go). In order:
//
//  1. the request: a name as campaignSaveTemplate takes one, a built-in
//     layout, a description of 1 to 8000 characters and at most 64 attached
//     Library files;
//  2. the organization: the caller may use it and write templates in it;
//  3. its brand kit: a composition writes into the organization's look, so an
//     organization that has none is sent to make one first (Start from the
//     layout still works without one);
//  4. an app the caller can reach: the goal runs on the Apps route and on no
//     other, so with no signed-in app on any of the caller's machines nothing
//     could run it, and the refusal says what to open;
//  5. the template, made from the layout with the kit applied, its composition
//     writing with the request (createTemplateFromLayout);
//  6. the goal (GoalStarter): composeEmailRun on route:apps at level strong,
//     requested via email, owned by the caller;
//  7. the goal recorded on the composition.
//
// The refusals of 1 to 4 write nothing. A goal that cannot be opened leaves the
// template, its composition failed, so that the person can retry it or write
// the email by hand, and the answer is campaignCompositionFailedToStart
// {templateId, revision, state, error}: it names the draft that was kept, so a
// client opens it rather than making another.
//
// # What a person reads
//
// Every sentence a person reads, in an answer or on the template, is a fixed
// one. The cause of a failure (the work runtime's words, a store's) is logged
// and never shown.
//
// # Who records the goal
//
// The goal's run may start before step 7, and it claims a writing composition
// that names no goal for its own goal (claimComposition, a compare and set
// under the template's lock). Step 7 is the same compare under the same lock:
// a composition that already names this goal is left as it is, since a second
// write would move the template past the revision the run's composition starts
// from and its submit would land beside the template instead of in it; one that
// names no goal, or another goal, is given this one. Either write keeps the
// composition's baseRevision in step with the row (rewriteComposition). A
// composition that is no longer this start's -- cancelled, or started again,
// while the goal was being opened -- is left alone, and the goal, which nothing
// will use, is cancelled. A record that cannot be written does not fail the
// start: the run claims its composition itself.
//
// The goal is opened, and what follows settles what the start wrote, on a
// context the caller's going away (a page closed mid-call) does not reach: an
// opening cut short could leave a goal with no run, and a composition left
// writing with no goal would wait for a run that never comes.
//
// # Cancelling
//
// Only a writing composition can be cancelled, and a cancel is held to the run
// it read. Its goal is asked to stop first (GoalCanceller: the work runtime's
// cancelGoal, as the caller), while the composition still reads writing, and
// then the composition is failed with "Cancelled", the word a cancelled run
// records itself. A composition its run settled meanwhile keeps its own record,
// and one started again meanwhile (a retry, a newer startedAt) is not this
// cancel's: neither it nor its goal is touched. A goal the same run recorded
// while the cancel was on its way is asked to stop as well. Once the cancel acts,
// it finishes whether or not the caller is still there. A goal that will not stop (one the caller
// cannot read, a work runtime that does not answer) does not keep the
// composition writing: a composition that is not writing is one no tool of that
// goal's run can write to (compose_tools.go). The draft stays as it was.
//
// # Retrying
//
// Only a failed composition can be retried, only by the person who asked for it
// (the request records them as requestedBy), and with the same checks as a start
// (organization, brand kit, an app). A retry reads the files the request names
// as whoever runs it, so a colleague's retry would read the colleague's own
// Library into a template the author can read; it is refused with a sentence,
// before anything is asked or written. Cancelling stays open to every writer.
//
// The composition is written as writing again with the request it recorded,
// from now, with no goal, no error and no notes; then a new goal is opened and
// recorded as a start's is. A run of the old goal that is still talking is
// refused by every tool, which holds a call to the goal the composition names.
//
// A retry keeps the revision the composition began from (its baseRevision).
// Every write of the composition alone keeps that revision in step with the
// template while nobody else changes it (rewriteComposition), so a retry over a
// draft nobody touched writes in place. A template somebody changed since --
// saved or published while MemQL wrote, or changed after a start that failed --
// is theirs, and the retried run's email is saved beside it as "<name> (MemQL's
// version)" (compose_tools.go), never over it. A person's own save or publish of
// a failed composition's template ends the failure instead (template.go), so
// there is nothing to retry.

const (
	// maxComposeDescriptionChars bounds what a composition is asked to write:
	// a person's description, not a document.
	maxComposeDescriptionChars = 8000

	// compositionGoalCancelReason is why the goal of a cancelled composition
	// closed, as the goal records it.
	compositionGoalCancelReason = "cancelled by person"

	// compositionGoalStartFailedReason is why a goal that opened for a
	// composition that could not start closed, as the goal records it.
	compositionGoalStartFailedReason = "the composition could not start"
)

var (
	errComposeNeedsBrand    = errors.New("Set up this organization's brand first: Email, Settings, Brand.")
	errComposeNeedsApp      = errors.New("Open Cockpit with Claude Code signed in, then try again.")
	errComposeAppsUnknown   = errors.New("MemQL could not check your apps right now. Try again in a moment.")
	errComposeCouldNotStart = errors.New("MemQL could not start writing this email. Try again in a moment.")
	errComposeDescription   = errors.New("Describe the email in 1 to 8000 characters.")
	errComposeFileID        = errors.New("Attach files from your Library.")
	errComposeUnavailable   = errors.New("MemQL can't write emails on this server. Ask your administrator.")
	errNothingToCancel      = errors.New("MemQL is not writing this email, so there is nothing to cancel.")
	errRetryNeedsFailed     = errors.New("Only an email MemQL could not finish can be retried.")
	errRetryNotYours        = errors.New("Only the person who asked MemQL to write this email can retry it. Use New email to write it with your own files.")

	// errNoGoalOpened is logged, never shown: the goal starter answered
	// without an error and without a goal.
	errNoGoalOpened = errors.New("the goal starter answered no goal id")
)

// handleComposeEmail implements integration.campaigns.composeEmail.
func (w *Worker) handleComposeEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	accountID := bare(strings.TrimSpace(argString(args, "accountId")))
	if accountID == "" || callerUserID(ctx) == "" {
		return nil, errChooseTemplate
	}
	req, err := composeRequest(args)
	if err != nil {
		return nil, err
	}
	req.RequestedBy = bare(callerUserID(ctx))
	if err := w.requireOrganization(ctx, accountID); err != nil {
		return nil, err
	}
	if !w.mayWriteTemplates(ctx, accountID) {
		return nil, errTemplateNeedsWrite
	}
	starter, err := w.readyToCompose(ctx, accountID)
	if err != nil {
		return nil, err
	}

	started := w.nowUTC().Truncate(time.Microsecond)
	c := composition{State: compositionWriting, StartedAt: started, Request: req}
	templateID, revision, _, err := w.createTemplateFromLayout(ctx, accountID, req.Name, req.LayoutID, &c)
	if err != nil {
		return nil, err
	}
	return w.openCompositionGoal(ctx, starter, compositionStart{
		templateID: templateID, accountID: accountID, name: req.Name, layoutID: req.LayoutID,
		startedAt: started, base: revision,
	})
}

// composeRequest reads what a composition is asked to write: the name as
// campaignSaveTemplate takes one, a built-in layout, the description, and the
// attached Library files with the picture of the finished email, each id bare.
func composeRequest(args map[string]any) (compositionRequest, error) {
	req := compositionRequest{
		Name:            strings.TrimSpace(argString(args, "name")),
		LayoutID:        strings.TrimSpace(argString(args, "layoutId")),
		Description:     strings.TrimSpace(argString(args, "description")),
		ReferenceFileID: bare(strings.TrimSpace(argString(args, "referenceFileId"))),
	}
	if req.Name == "" || utf8.RuneCountInString(req.Name) > maxTemplateNameChars || strings.IndexFunc(req.Name, unicode.IsControl) >= 0 {
		return compositionRequest{}, errChooseTemplate
	}
	_, known := emaildoc.LayoutMeta(req.LayoutID)
	_, built := emaildoc.Layout(req.LayoutID)
	if !known || !built {
		return compositionRequest{}, errUnknownLayout
	}
	if n := utf8.RuneCountInString(req.Description); n == 0 || n > maxComposeDescriptionChars {
		return compositionRequest{}, errComposeDescription
	}
	files, err := composeFileIDs(args["fileIds"])
	if err != nil {
		return compositionRequest{}, err
	}
	req.FileIDs = files
	if req.ReferenceFileID != "" && !templateIDPattern.MatchString(req.ReferenceFileID) {
		return compositionRequest{}, errComposeFileID
	}
	return req, nil
}

// composeFileIDs reads the attached files: Library file ids, each kept once and
// bare, blanks left out, and at most maxMaterialFiles of them (the bound the
// material is read with, compose_material.go).
func composeFileIDs(raw any) ([]string, error) {
	var list []any
	switch v := raw.(type) {
	case nil:
	case []any:
		list = v
	case []string:
		list = make([]any, len(v))
		for i, id := range v {
			list[i] = id
		}
	default:
		return nil, errComposeFileID
	}
	ids, seen := []string{}, map[string]bool{}
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, errComposeFileID
		}
		id := bare(strings.TrimSpace(text))
		switch {
		case id == "" || seen[id]:
			continue
		case !templateIDPattern.MatchString(id):
			return nil, errComposeFileID
		case len(ids) == maxMaterialFiles:
			return nil, errTooManyMaterialFiles
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// readyToCompose checks what a composition needs before anything is written
// for it: the organization's brand kit, a node that can open its goal, and an
// app of the caller's to run it on. It answers the goal starter.
func (w *Worker) readyToCompose(ctx context.Context, accountID string) (GoalStarter, error) {
	_, _, hasKit, err := w.brandKitFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !hasKit {
		return nil, errComposeNeedsBrand
	}
	starter, _, apps := w.compositionGoals()
	if starter == nil || apps == nil {
		if w.logger != nil {
			w.logger.Warn("campaigns: a composition cannot start on this node: its goal starter or app check is not wired",
				"starter", starter != nil, "apps", apps != nil)
		}
		return nil, errComposeUnavailable
	}
	reachable, err := apps.HasReachableApp(ctx, callerUserID(ctx))
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("campaigns: a composition could not ask whether the caller has an app", "account", accountID, "error", err)
		}
		return nil, errComposeAppsUnknown
	}
	if !reachable {
		return nil, errComposeNeedsApp
	}
	return starter, nil
}

// compositionStart is one start of a template's composition, as it was written
// as writing: the template, its organization and name, the layout, when the
// composition started, and the revision it starts from.
type compositionStart struct {
	templateID, accountID, name, layoutID string
	startedAt, base                       time.Time
}

// openCompositionGoal opens the goal of the composition s wrote and records
// it, on a context the caller's going away does not reach (see the header), and
// answers campaignCompositionStarted {templateId, goalId, revision, state}, the
// state being the composition's as the call leaves it. A goal that cannot be
// opened fails the composition (failStart).
func (w *Worker) openCompositionGoal(ctx context.Context, starter GoalStarter, s compositionStart) ([]memorynodes.MemoryNode, error) {
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), compositionFinalWait)
	defer cancel()
	goalID, err := starter.StartComposition(settle, CompositionGoal{
		OwnerUserID: callerUserID(ctx), AccountID: s.accountID, TemplateID: s.templateID,
		Statement: compositionStatement(s.name, s.layoutID),
	})
	goalID = strings.TrimSpace(goalID)
	if err == nil && goalID == "" {
		err = errNoGoalOpened
	}
	if err != nil {
		return w.failStart(settle, s, goalID, err)
	}
	c, revision, ours, err := w.recordGoal(settle, s, goalID)
	state := c.State
	switch {
	case err != nil:
		if w.logger != nil {
			w.logger.Warn("campaigns: a composition's goal could not be recorded; its run claims the composition itself",
				"template", s.templateID, "goal", goalID, "error", err)
		}
		revision, state = s.base, compositionWriting
	case !ours:
		w.stopGoal(settle, goalID, compositionGoalCancelReason)
	}
	return resultNode("campaignCompositionStarted", map[string]any{
		"templateId": s.templateID, "goalId": goalID, "revision": revision.UTC().Format(time.RFC3339Nano), "state": state,
	})
}

// compositionStatement is what a composition's goal is shown as in Nexus.
func compositionStatement(name, layoutID string) string {
	statement := `Write the email "` + name + `"`
	if info, ok := emaildoc.LayoutMeta(layoutID); ok && info.Name != "" {
		statement += " from the " + info.Name + " layout"
	}
	return statement
}

// recordGoal records goalID on the composition s began (see the header) and
// answers the composition and the template's revision as it leaves them, and
// whether the composition is this goal's: false when it is no longer s's start,
// which is left as it is.
func (w *Worker) recordGoal(ctx context.Context, s compositionStart, goalID string) (composition, time.Time, bool, error) {
	ours := false
	c, revision, err := w.updateComposition(ctx, s.accountID, s.templateID, func(c *composition, has bool, _ map[string]any) (bool, error) {
		switch {
		case has && sameGoal(c.GoalID, goalID):
			// Its run recorded it first.
			ours = true
			return false, nil
		case !has || c.State != compositionWriting || !c.StartedAt.Equal(s.startedAt):
			return false, nil
		}
		ours = true
		c.GoalID = goalID
		if c.BaseRevision.IsZero() {
			c.BaseRevision = s.base
		}
		return true, nil
	})
	return c, revision, ours, err
}

// failStart fails the composition s began because its goal could not be
// opened. The cause is logged and never shown: the composition records, and
// the answer says, errComposeCouldNotStart. The composition records the
// revision it began from, as a start that opened its goal does (recordGoal),
// so that a retry after somebody changed the template is not taken for one
// over the template as it began. A goal the work runtime answered together with
// the error (opened, its run not started) is stopped: nothing will use it. It
// answers campaignCompositionFailedToStart {templateId, revision, state,
// error}, naming the draft the start kept, so that a client opens it (where the
// person can retry) rather than making another.
func (w *Worker) failStart(ctx context.Context, s compositionStart, goalID string, cause error) ([]memorynodes.MemoryNode, error) {
	if w.logger != nil {
		w.logger.Warn("campaigns: a composition's goal could not be opened", "template", s.templateID, "goal", goalID, "error", cause)
	}
	reason := errComposeCouldNotStart.Error()
	c, revision, err := w.updateComposition(ctx, s.accountID, s.templateID, func(c *composition, has bool, _ map[string]any) (bool, error) {
		if !has || c.State != compositionWriting || !c.StartedAt.Equal(s.startedAt) {
			return false, nil
		}
		c.State, c.Error, c.FinishedAt = compositionFailed, reason, w.nowUTC()
		if c.BaseRevision.IsZero() {
			c.BaseRevision = s.base
		}
		return true, nil
	})
	if goalID != "" {
		w.stopGoal(ctx, goalID, compositionGoalStartFailedReason)
	}
	state := c.State
	if err != nil {
		// It stays writing with no goal, until a person cancels it.
		if w.logger != nil {
			w.logger.Warn("campaigns: a composition whose goal could not be opened could not be marked failed", "template", s.templateID, "error", err)
		}
		revision, state = s.base, compositionWriting
	}
	return resultNode("campaignCompositionFailedToStart", map[string]any{
		"templateId": s.templateID, "revision": revision.UTC().Format(time.RFC3339Nano), "state": state, "error": reason,
	})
}

// handleCancelComposition implements integration.campaigns.cancelComposition.
func (w *Worker) handleCancelComposition(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	templateID, err := compositionTemplateArg(ctx, args)
	if err != nil {
		return nil, err
	}
	row, err := w.templateToCompose(ctx, templateID, "")
	if err != nil {
		return nil, err
	}
	seen, has := compositionFromRow(row)
	if !has || seen.State != compositionWriting {
		return nil, errNothingToCancel
	}
	// From here the cancel finishes even if the caller goes away: a goal asked
	// to stop whose run had not started would otherwise leave the composition
	// writing.
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), compositionFinalWait)
	defer cancel()
	stopped := map[string]bool{}
	if seen.GoalID != "" {
		w.stopGoal(settle, seen.GoalID, compositionGoalCancelReason)
		stopped[bare(seen.GoalID)] = true
	}
	var writingFor string
	c, revision, err := w.updateComposition(settle, bare(str(row, "accountId")), templateID, func(c *composition, has bool, _ map[string]any) (bool, error) {
		// Held to the run it read: one its run settled meanwhile keeps its
		// record, and one started again meanwhile is not this cancel's.
		if !has || c.State != compositionWriting || !c.StartedAt.Equal(seen.StartedAt) {
			return false, nil
		}
		writingFor = c.GoalID
		c.State, c.Error, c.FinishedAt = compositionFailed, compositionCancelled, w.nowUTC()
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if writingFor != "" && !stopped[bare(writingFor)] {
		w.stopGoal(settle, writingFor, compositionGoalCancelReason)
	}
	return resultNode("campaignCompositionCancelled", map[string]any{
		"templateId": templateID, "revision": revision.UTC().Format(time.RFC3339Nano), "state": c.State,
	})
}

// handleRetryComposition implements integration.campaigns.retryComposition.
func (w *Worker) handleRetryComposition(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	templateID, err := compositionTemplateArg(ctx, args)
	if err != nil {
		return nil, err
	}
	row, err := w.templateToCompose(ctx, templateID, "")
	if err != nil {
		return nil, err
	}
	accountID := bare(str(row, "accountId"))
	c, has := compositionFromRow(row)
	if !has || c.State != compositionFailed {
		return nil, errRetryNeedsFailed
	}
	if !askedBy(ctx, c) {
		return nil, errRetryNotYours
	}
	starter, err := w.readyToCompose(ctx, accountID)
	if err != nil {
		return nil, err
	}

	started := w.nowUTC().Truncate(time.Microsecond)
	s := compositionStart{templateID: templateID, accountID: accountID, startedAt: started}
	_, revision, err := w.updateComposition(ctx, accountID, templateID, func(c *composition, has bool, row map[string]any) (bool, error) {
		switch {
		case !has || c.State != compositionFailed:
			return false, errRetryNeedsFailed
		case !askedBy(ctx, *c):
			return false, errRetryNotYours
		}
		// The revision it began from stays (see the header): a template
		// somebody changed since gets MemQL's version beside it.
		c.State, c.Error, c.GoalID, c.Notes = compositionWriting, "", "", nil
		c.StartedAt, c.FinishedAt = started, time.Time{}
		s.name, s.layoutID = str(row, "name"), c.Request.LayoutID
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	s.base = revision
	return w.openCompositionGoal(ctx, starter, s)
}

// askedBy reports whether the caller is the person who asked for the
// composition, whichever way either id is written. A composition that does
// not say who asked is nobody's.
func askedBy(ctx context.Context, c composition) bool {
	asker := bare(strings.TrimSpace(c.Request.RequestedBy))
	return asker != "" && asker == bare(strings.TrimSpace(callerUserID(ctx)))
}

// compositionTemplateArg is the template a cancel or a retry names.
func compositionTemplateArg(ctx context.Context, args map[string]any) (string, error) {
	id := bare(strings.TrimSpace(argString(args, "templateId")))
	if id == "" || !templateIDPattern.MatchString(id) || callerUserID(ctx) == "" {
		return "", errComposeTemplateMissing
	}
	return id, nil
}

// templateToCompose reads the template a start, a cancel or a retry acts on,
// as the caller: it must exist, belong to accountID when one is given, and the
// caller must be able to use its organization and write templates in it.
func (w *Worker) templateToCompose(ctx context.Context, templateID, accountID string) (map[string]any, error) {
	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", templateID}))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errComposeTemplateMissing
	}
	row := rows[0]
	stored := bare(str(row, "accountId"))
	if stored == "" {
		return nil, errComposeTemplateMissing
	}
	if accountID != "" && !sameOrganization(accountID, stored) {
		return nil, errTemplateOrgMoved
	}
	if err := w.requireOrganization(ctx, stored); err != nil {
		return nil, err
	}
	if !w.mayWriteTemplates(ctx, stored) {
		return nil, errTemplateNeedsWrite
	}
	return row, nil
}

// updateComposition changes a template's composition in one row version under
// the template's lock. The row is read fresh and held to accountID and to the
// caller's organization and permission (templateToCompose); change is handed
// the stored composition (has is false when the template has none) and the
// row, and when it answers true the composition is written as change left it
// (rewriteComposition). It answers the composition and the template's revision
// as the call leaves them.
func (w *Worker) updateComposition(ctx context.Context, accountID, templateID string, change func(c *composition, has bool, row map[string]any) (bool, error)) (composition, time.Time, error) {
	if w.templateGate == nil {
		return composition{}, time.Time{}, errTemplateGateUnavailable
	}
	release, err := w.lockTemplate(ctx, templateID)
	if err != nil {
		return composition{}, time.Time{}, err
	}
	defer release()
	row, err := w.templateToCompose(memql.ContextWithFreshRead(ctx), templateID, accountID)
	if err != nil {
		return composition{}, time.Time{}, err
	}
	c, has := compositionFromRow(row)
	revision := templateTime(row["createdAt"])
	write, err := change(&c, has, row)
	if err != nil || !write {
		return c, revision, err
	}
	if revision, err = w.rewriteComposition(ctx, templateID, row, c); err != nil {
		return composition{}, time.Time{}, err
	}
	return c, revision, nil
}

// stopGoal asks a composition's goal to stop, as the caller, for reason. It
// never fails the call that asks: the composition is the record, and once it is
// not writing no tool of that goal's run can write to the template, so a goal
// that will not stop keeps only a session whose tools are refused.
func (w *Worker) stopGoal(ctx context.Context, goalID, reason string) {
	_, canceller, _ := w.compositionGoals()
	if canceller == nil {
		if w.logger != nil {
			w.logger.Warn("campaigns: this node cannot cancel a composition's goal: it has no work runtime wired", "goal", goalID)
		}
		return
	}
	if err := canceller.CancelGoal(ctx, goalID, reason); err != nil && w.logger != nil {
		w.logger.Warn("campaigns: a composition's goal could not be cancelled; its tools refuse it all the same", "goal", goalID, "error", err)
	}
}
