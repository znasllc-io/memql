package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	pure "github.com/znasllc-io/memql/component/compose"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
	work "github.com/znasllc-io/memql/integrations/work"
)

// materialize.go -- the five steps, in order.
//
//	gather      deterministic   resolve every source ref to rows/bytes
//	compose     REASONING       the one model call: rows + template -> the draft
//	render      deterministic   draft + template -> the format's bytes
//	stamp       deterministic   embed provenance, per the format's table
//	file        deterministic   write v1:library:file + the composition record
//
// The known template needs no compiler call. Its compose stage reaches the
// configured model and the work runtime journals that call on the owning run.
// Gathered inputs and completed output identities survive recovery on another
// replica through the composition and its owner-only input record.
//
// THE WHOLE PATH RUNS UNDER THE CALLER'S OWN ACTOR and borrows nobody's
// authority -- everything it touches is the caller's: their sources,
// their template, their Library folder. That is a stronger position than
// the campaigns drain worker's and it is worth keeping, because it means
// a template a caller cannot read is REFUSED rather than rendered
// through, and a source they cannot read simply does not come back.

// materializeArgs is the decoded capability argument set.
type materializeArgs struct {
	Name           string
	Statement      string
	Format         pure.Format
	Sources        []SourceRef
	Draft          string
	TemplateId     string
	FolderId       string
	AccountIds     []string
	DeployableKind string
	RecipeId       string
	Ceilings       map[string]any
}

func (i *Integration) handleMaterialize(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	parsed, err := parseMaterializeArgs(args)
	if err != nil {
		return nil, err
	}
	out, err := i.materialize(ctx, ac.UserId, ac.PrimaryEmail, parsed)
	if err != nil {
		return nil, err
	}
	return i.resultNode(out), nil
}

func parseMaterializeArgs(args map[string]any) (materializeArgs, error) {
	out := materializeArgs{
		Name:           strings.TrimSpace(stringOf(args["name"])),
		Statement:      strings.TrimSpace(stringOf(args["statement"])),
		Draft:          stringOf(args["draft"]),
		TemplateId:     strings.TrimSpace(stringOf(args["templateId"])),
		FolderId:       strings.TrimSpace(stringOf(args["folderId"])),
		DeployableKind: strings.TrimSpace(stringOf(args["deployableKind"])),
		RecipeId:       strings.TrimSpace(stringOf(args["recipeId"])),
		AccountIds:     stringList(args["accountIds"]),
	}
	if out.Name == "" {
		return out, fmt.Errorf("compose: a materialization needs a name")
	}
	format, err := pure.ParseFormat(stringOf(args["format"]))
	if err != nil {
		return out, err
	}
	out.Format = format
	if out.DeployableKind != "" {
		if _, err := pure.ParseDeployableKind(out.DeployableKind); err != nil {
			return out, err
		}
	}
	refs, err := parseSourceRefs(args["sources"])
	if err != nil {
		return out, err
	}
	out.Sources = refs
	if m, ok := args["ceilings"].(map[string]any); ok {
		out.Ceilings = m
	}
	return out, nil
}

// executionRequest is captured once under the requesting actor. Retrying on
// another replica reads this snapshot rather than asking mutable sources again.
type executionRequest struct {
	Args     materializeArgs `json:"args"`
	Resolved []Resolved      `json:"resolved"`
	Started  time.Time       `json:"started"`
}

var materializeIDEngine = id.NewUntracked()

func stableMaterializeID(parts ...string) string {
	return string(materializeIDEngine.MustFromMap(map[string]any{"parts": parts}))[:32]
}

func (i *Integration) materialize(ctx context.Context, userId, userEmail string, a materializeArgs) (map[string]any, error) {
	rc, nested := common.RunFromContext(ctx)
	nested = nested && rc.RunId != "" && rc.GoalId != ""
	compositionId := id.NewShortId()
	if nested {
		// The step owns a single materialization for this exact request. Including
		// args also distinguishes multiple file calls within the same agent step.
		raw, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		identityRun := rc.RunId
		if rc.Mode == common.RunModeReplay {
			if rc.SourceRunId == "" || !sameRowID(rc.SourceGoalId, rc.GoalId) {
				return nil, errors.New("compose: replay needs a source run in the same goal")
			}
			identityRun = rc.SourceRunId
		}
		compositionId = stableMaterializeID(memql.BareShortId(identityRun), rc.StepKey, string(raw))
		row, err := i.store().compositionExecutionById(ctx, compositionId)
		if err != nil {
			return nil, err
		}
		if row != nil {
			return i.executeComposition(ctx, userId, userEmail, compositionId, row)
		}
		if rc.Mode == common.RunModeReplay {
			return nil, errors.New("compose: replay has no completed composition to serve")
		}
	}
	resolved, err := i.resolve(ctx, a.Sources)
	if err != nil {
		return nil, fmt.Errorf("compose: the sources could not be read: %w", err)
	}
	request := executionRequest{Args: a, Resolved: resolved, Started: i.clock().UTC()}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("compose: capturing the request: %w", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	goalId, runId := "", ""
	if nested {
		goalId, runId = rc.GoalId, rc.RunId
	}
	// A composition may be shared with an account, but the source payloads
	// were resolved using its owner's access. Persist them privately before
	// making a composition available to the executor on any replica.
	if err := i.store().createCompositionInput(ctx, map[string]any{
		"compositionId": compositionId, "request": snapshot,
	}); err != nil {
		return nil, fmt.Errorf("compose: saving the execution input: %w", err)
	}
	if err := i.store().createComposition(ctx, map[string]any{
		"compositionId": compositionId, "name": a.Name, "statement": a.Statement,
		"format": string(a.Format), "sources": rowSources(resolved),
		"templateId": a.TemplateId, "folderId": a.FolderId, "accountIds": stringsOrNil(a.AccountIds),
		"goalId": goalId, "runId": runId, "recipeId": a.RecipeId, "deployableKind": a.DeployableKind,
	}); err != nil {
		return nil, fmt.Errorf("compose: opening the composition record: %w", err)
	}
	if nested {
		row, err := i.store().compositionExecutionById(ctx, compositionId)
		if err != nil {
			return nil, err
		}
		return i.executeComposition(ctx, userId, userEmail, compositionId, row)
	}
	opener := i.goalOpenerRef()
	if opener == nil {
		return i.failComposition(ctx, compositionId, errors.New("compose: the work runtime is not configured"))
	}
	goalId, runId, err = opener.OpenDirectGoal(ctx, work.DirectGoal{
		OwnerUserId: userId, Statement: firstNonEmpty(a.Statement, "Materialize "+a.Name+" as "+string(a.Format)),
		AutomationName: "materializeFile", RequestedVia: "materializer", TriggeredBy: "materializer",
		AccountIds: a.AccountIds, Ceilings: a.Ceilings, Input: map[string]any{"compositionId": compositionId},
		BeforeRun: func(bindCtx context.Context, goal, run string) error {
			return i.store().updateCompositionState(bindCtx, map[string]any{"compositionId": compositionId, "goalId": goal, "runId": run})
		},
	})
	if err != nil {
		return i.failComposition(ctx, compositionId, fmt.Errorf("compose: starting the work run: %w", err))
	}
	return map[string]any{"compositionId": compositionId, "goalId": goalId, "runId": runId, "format": string(a.Format), "status": "draft"}, nil
}

// handleExecute is reachable only in a run carrying this composition's identities.
// @serverOnly is unsuitable here: adopted runs intentionally retain the caller's
// origin even for trusted templates. The run context is server supplied and is
// checked together with the persisted owner before reading the execution input.
func (i *Integration) handleExecute(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.RunId == "" || rc.GoalId == "" {
		return nil, errors.New("compose: execution requires its owning work run")
	}
	compositionId := strings.TrimSpace(stringOf(args["compositionId"]))
	if compositionId == "" {
		return nil, errors.New("compose: execution needs a compositionId")
	}
	row, err := i.store().compositionExecutionById(ctx, compositionId)
	if err != nil {
		return nil, err
	}
	out, err := i.executeComposition(ctx, ac.UserId, ac.PrimaryEmail, compositionId, row)
	if err != nil {
		return nil, err
	}
	return i.resultNode(out), nil
}

func sameRowID(a, b string) bool {
	// Persisted relationship fields are canonical, while a rehydrated work
	// journal deliberately exposes its run's bare ID. Both name the same row.
	return a != "" && b != "" && memql.BareShortId(a) == memql.BareShortId(b)
}
func (i *Integration) executeComposition(ctx context.Context, userId, userEmail, compositionId string, row map[string]any) (map[string]any, error) {
	if row == nil {
		return nil, errors.New("compose: no composition with that id is readable by you")
	}
	rc, ok := common.RunFromContext(ctx)
	sameRun := sameRowID(rc.RunId, stringOf(row["runId"]))
	replaySource := rc.Mode == common.RunModeReplay && sameRowID(rc.SourceRunId, stringOf(row["runId"])) && sameRowID(rc.SourceGoalId, rc.GoalId)
	if !ok || rc.GoalId == "" || rc.RunId == "" || (!sameRun && !replaySource) || !sameRowID(rc.GoalId, stringOf(row["goalId"])) || !sameRowID(userId, stringOf(row["ownerUserId"])) {
		return nil, errors.New("compose: this composition does not belong to the current caller and work run")
	}
	if stringOf(row["status"]) == "ready" {
		return compositionResult(compositionId, row), nil
	}
	if replaySource {
		return nil, errors.New("compose: replay source did not complete; no file can be served")
	}
	if stringOf(row["status"]) == "cancelled" {
		return nil, errors.New("compose: the composition was cancelled")
	}
	input, err := i.store().compositionInputById(ctx, compositionId)
	if err != nil {
		return nil, err
	}
	var request executionRequest
	raw, err := json.Marshal(input["request"])
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	if request.Args.Name == "" {
		return i.failComposition(ctx, compositionId, errors.New("compose: the saved execution request is missing"))
	}
	return i.executePipeline(ctx, userId, userEmail, compositionId, rc.GoalId, rc.RunId, request)
}
func compositionResult(compositionId string, row map[string]any) map[string]any {
	out := map[string]any{"compositionId": compositionId}
	for _, key := range []string{"goalId", "runId", "outputFileId", "format", "name", "status", "sha256", "provenanceEmbedded", "provenanceNote", "modelsUsed", "deployableKind"} {
		if value, ok := row[key]; ok {
			out[key] = value
		}
	}
	return out
}

// terminalFailureCode is the word this integration puts at the head of the
// error it returns once the composition row is terminally `failed`.
//
// IT IS A CONTRACT WITH component/work, which matches it in
// TerminalFailureCode and takes the run terminal rather than parking it on a
// retry. The two modules cannot see each other -- this one raises the failure,
// and the executor that decides what the run does about it sees only the
// string the executor recorded -- so the code is a stable word carried in the
// message, asserted at both ends by their own tests. That is the same contract
// component/router has with work.InferenceRefusalCode.
//
// Without it, the failure this integration already recorded as final arrived
// at the symptom table as prose. "context deadline exceeded" matched
// transient.timeout, the run parked at `waiting`, and a person watching it
// waited on a composition the database had already given up on.
const terminalFailureCode = "composition_failed"

func (i *Integration) failComposition(ctx context.Context, compositionId string, cause error) (map[string]any, error) {
	// Cancellation may invalidate ctx while the model is in flight. The terminal
	// record still needs a bounded write under the same actor.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	row, readErr := i.store().compositionById(cleanup, compositionId)
	status := "failed"
	if (row != nil && stringOf(row["status"]) == "cancelled") || errors.Is(cause, context.Canceled) {
		status = "cancelled"
	}
	if readErr != nil {
		i.log().Warn("compose: could not read terminal composition state", "error", readErr)
	}
	if err := i.store().updateCompositionState(cleanup, map[string]any{"compositionId": compositionId, "status": status, "failureReason": cause.Error()}); err != nil {
		i.log().Error("compose: could not record composition failure", "compositionId", compositionId, "error", err)
	}
	if status == "cancelled" {
		// A CANCELLATION IS NOT THIS CODE. Somebody asked it to stop, which
		// the work spine already has a state for, and calling it a terminal
		// failure would report a person's own click back to them as a fault.
		return nil, cause
	}
	return nil, fmt.Errorf("%s: %w", terminalFailureCode, cause)
}

func (i *Integration) checkCompositionCancellation(ctx context.Context, compositionId string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	row, err := i.store().compositionById(ctx, compositionId)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("compose: the composition is no longer readable")
	}
	if stringOf(row["status"]) == "cancelled" {
		return errors.New("compose: the composition was cancelled")
	}
	return nil
}

func (i *Integration) executePipeline(ctx context.Context, userId, userEmail, compositionId, goalId, runId string, request executionRequest) (map[string]any, error) {
	st := i.store()
	a, resolved, started := request.Args, request.Resolved, request.Started
	fail := func(reason string, cause error) (map[string]any, error) {
		if cause == nil {
			cause = errors.New("compose: " + reason)
		} else {
			cause = fmt.Errorf("compose: %s: %w", reason, cause)
		}
		return i.failComposition(ctx, compositionId, cause)
	}
	if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
		return fail("materialization stopped", err)
	}

	existingFileId := stableMaterializeID("materialized-file", compositionId)
	existingFile, err := st.libraryFileById(ctx, existingFileId)
	if err != nil {
		return fail("checking an earlier output failed", err)
	}
	if existingFile != nil {
		// The filing row is written only after uploading all bytes. Its identity is
		// stable even when the final composition update was interrupted.
		if err := st.updateCompositionState(ctx, map[string]any{"compositionId": compositionId, "status": "ready", "outputFileId": existingFileId, "sha256": existingFile["sha256"]}); err != nil {
			return nil, err
		}
		row, err := st.compositionById(ctx, compositionId)
		if err != nil {
			return nil, err
		}
		return compositionResult(compositionId, row), nil
	}

	// --- the template, resolved under the caller ---
	var templateName, templateBody string
	if a.TemplateId != "" {
		row, terr := st.templateById(ctx, a.TemplateId)
		if terr != nil {
			return fail("the template could not be read: "+terr.Error(), terr)
		}
		if row == nil {
			// REFUSED RATHER THAN RENDERED WITHOUT. A template the
			// caller cannot read is one they may not use, and silently
			// producing an unbranded document that looks finished is
			// worse than a refusal naming the template.
			return fail("that template is not readable by you, so nothing was rendered through it", nil)
		}
		templateName = stringOf(row["name"])
		if fileId := strings.TrimSpace(stringOf(row["fileId"])); fileId != "" {
			templateBody = i.templateBody(ctx, fileId)
		}
	}

	if err := st.updateCompositionState(ctx, map[string]any{
		"compositionId": compositionId, "status": "composing", "runId": runId,
	}); err != nil {
		i.log().Warn("compose: could not mark the composition composing", "error", err, "compositionId", compositionId)
	}

	// --- step 2: compose (THE ONE REASONING STEP) ---
	draft := pure.Draft{Title: a.Name, Body: a.Draft}
	var models []pure.ModelContribution
	switch composer := i.composerRef(); {
	case composer != nil:
		reply, cerr := composer.Compose(ctx, ComposeRequest{
			Statement:    a.Statement,
			Format:       a.Format,
			Sources:      i.narrowed(resolved),
			TemplateName: templateName,
			TemplateBody: templateBody,
			Draft:        a.Draft,
		})
		if cerr != nil {
			return fail("composing the draft failed: "+cerr.Error(), cerr)
		}
		draft = reply.Draft
		models = reply.Models
		if strings.TrimSpace(draft.Title) == "" {
			draft.Title = a.Name
		}
	case strings.TrimSpace(a.Draft) != "":
		// NO COMPOSER AND A SUPPLIED DRAFT is a complete, honest run --
		// and it is what lets the render/stamp/file tail be exercised on
		// a node with no provider configured. `models` stays EMPTY,
		// which is the truthful record: nothing thought.
	default:
		return fail("this node has no composer configured and no draft was supplied, so there is nothing to render", nil)
	}

	if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
		return fail("materialization stopped", err)
	}

	// The two data formats take their rows from the sources rather than
	// from prose, and a composer that returned none is not an error --
	// it means the draft's body was the interesting part and the rows
	// are the sources'.
	if (a.Format == pure.FormatCSV || a.Format == pure.FormatJSON) && len(draft.Rows) == 0 {
		draft.Header, draft.Rows = tabularRows(resolved)
	}

	if err := st.updateCompositionState(ctx, map[string]any{
		"compositionId": compositionId, "status": "rendering",
	}); err != nil {
		i.log().Warn("compose: could not mark the composition rendering", "error", err, "compositionId", compositionId)
	}

	if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
		return fail("materialization stopped", err)
	}

	// --- steps 3 + 4: render and stamp ---
	prov := pure.Provenance{
		Title:         a.Name,
		Statement:     a.Statement,
		AuthorName:    firstNonEmpty(userEmail, userId),
		AuthorId:      userId,
		Instance:      i.instanceRef(),
		CompositionId: compositionId,
		GoalId:        goalId,
		TemplateName:  templateName,
		Sources:       pureSources(resolved),
		Models:        models,
		CreatedAt:     started,
	}

	var rendered pure.Result
	if a.DeployableKind != "" {
		rendered, err = i.renderDeployable(a, draft, prov)
	} else {
		rendered, err = pure.Render(a.Format, draft, prov)
	}
	if err != nil {
		return fail("rendering the file failed: "+err.Error(), err)
	}

	if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
		return fail("materialization stopped", err)
	}

	// --- step 5: file ---
	fileId := stableMaterializeID("materialized-file", compositionId)
	fileName := outputFileName(a.Name, a.Format, a.DeployableKind)
	mimeType := a.Format.MimeType()
	if a.DeployableKind != "" {
		mimeType = "application/zip"
	}

	if err := st.updateCompositionState(ctx, map[string]any{
		"compositionId": compositionId, "modelsUsed": modelRows(models), "provenanceEmbedded": rendered.Embedded,
		"provenanceNote": rendered.Note, "sha256": rendered.SHA256(),
	}); err != nil {
		return fail("recording output provenance failed", err)
	}

	blobUrl, storageErr := i.storeBytes(ctx, userId, fileId, fileName, mimeType, rendered.Bytes)
	if storageErr != "" {
		// THE ROW IS STILL WRITTEN AND THEN MARKED FAILED, which is the
		// Library upload route's own shape: the owner has to be able to
		// SEE that their materialization did not store, and what is
		// never written is a placeholder that reads to every consumer
		// as a successfully stored file.
		return fail(storageErr, nil)
	}

	if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
		return fail("materialization stopped", err)
	}
	runContext, _ := common.RunFromContext(ctx)
	if err := st.createLibraryFile(ctx, map[string]any{
		"fileId":            fileId,
		"name":              fileName,
		"mimeType":          mimeType,
		"size":              len(rendered.Bytes),
		"sha256":            rendered.SHA256(),
		"blobUrl":           blobUrl,
		"source":            "agent_generated",
		"producedByRunId":   runId,
		"producedByStepKey": runContext.StepKey,
		"format":            libraryFormatFor(a.Format, a.DeployableKind),
		"summary":           fileSummary(a, prov),
		"folderId":          a.FolderId,
	}); err != nil {
		return fail("the output could not be filed in your Library: "+err.Error(), err)
	}
	if err := st.setLibraryFileReady(ctx, fileId, fileSummary(a, prov)); err != nil {
		// The native work step completes only after the durable delivery
		// receipt exists. A stored blob alone cannot mark the run successful.
		return fail("the output file could not be marked ready", err)
	}

	if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
		return fail("materialization stopped", err)
	}

	if err := st.updateCompositionState(ctx, map[string]any{
		"compositionId":      compositionId,
		"status":             "ready",
		"outputFileId":       fileId,
		"modelsUsed":         modelRows(models),
		"provenanceEmbedded": rendered.Embedded,
		"provenanceNote":     rendered.Note,
		"sha256":             rendered.SHA256(),
	}); err != nil {
		return nil, fmt.Errorf("compose: the file was written but the record could not be completed: %w", err)
	}

	if a.RecipeId != "" {
		i.bumpRecipe(ctx, a.RecipeId)
	}

	return map[string]any{
		"compositionId":      compositionId,
		"goalId":             goalId,
		"runId":              runId,
		"outputFileId":       fileId,
		"name":               fileName,
		"format":             string(a.Format),
		"deployableKind":     a.DeployableKind,
		"sizeBytes":          len(rendered.Bytes),
		"sha256":             rendered.SHA256(),
		"provenanceEmbedded": rendered.Embedded,
		"provenanceNote":     rendered.Note,
		"modelsUsed":         modelRows(models),
		"sourcesResolved":    len(resolved),
	}, nil
}

// renderDeployable produces the package source zip (design D8).
//
// The draft's body is the app's index document. That is deliberately
// small: a materialized deployable is a page this cluster composed, not
// a source tree with a toolchain, so `build` stays empty and the
// Deployables rail draws Build SKIPPED with "its built output is in the
// source" -- a reading it already has and already explains.
func (i *Integration) renderDeployable(a materializeArgs, draft pure.Draft, prov pure.Provenance) (pure.Result, error) {
	kind, err := pure.ParseDeployableKind(a.DeployableKind)
	if err != nil {
		return pure.Result{}, err
	}
	page, err := pure.Render(pure.FormatHTML, draft, prov)
	if err != nil {
		return pure.Result{}, err
	}
	return pure.BuildPackageSource(pure.PackageSource{
		Name: a.Name,
		Deployables: []pure.Deployable{{
			Name:  a.Name,
			Kind:  kind,
			Files: []pure.DeployableFile{{Path: "index.html", Body: page.Bytes}},
		}},
	}, prov)
}

// narrowed applies each concept's @composable(fields=...) projection to
// the rows it resolved.
//
// IT IS DONE HERE RATHER THAN IN THE READ because the read is the
// concept's own declared query and its shape is not ours to change. A
// compose prompt handed forty fields spends its context on ids, which is
// the whole reason the annotation carries a field list.
func (i *Integration) narrowed(resolved []Resolved) []Resolved {
	out := make([]Resolved, 0, len(resolved))
	for _, r := range resolved {
		if r.Ref.Kind != KindConceptRow || len(r.Rows) == 0 {
			out = append(out, r)
			continue
		}
		conceptId, _, ok := splitConceptRef(r.Ref.Ref)
		if !ok {
			out = append(out, r)
			continue
		}
		fields := i.fieldsFor(conceptId)
		if len(fields) == 0 {
			out = append(out, r)
			continue
		}
		narrowed := make([]map[string]any, 0, len(r.Rows))
		for _, row := range r.Rows {
			slim := make(map[string]any, len(fields)+1)
			// `id` always travels, whatever the projection says: a row
			// with no id in a provenance context is one nobody can go
			// back to.
			if v, ok := row["id"]; ok {
				slim["id"] = v
			}
			for _, f := range fields {
				if v, ok := row[f]; ok {
					slim[f] = v
				}
			}
			narrowed = append(narrowed, slim)
		}
		r.Rows = narrowed
		out = append(out, r)
	}
	return out
}

// templateBody reads a template file's bytes.
//
// A TEMPLATE THAT CANNOT BE READ IS EMPTY RATHER THAN FATAL, and the
// distinction from an UNREADABLE TEMPLATE ROW above is the point: the
// row is an authorization question and its refusal must stop the run,
// while the bytes are an availability question -- blob storage down,
// a file still uploading -- and a composition that proceeds with the
// template's NAME but not its content is a degraded answer rather than
// a wrong one.
func (i *Integration) templateBody(ctx context.Context, fileId string) string {
	row, err := i.store().libraryFileById(ctx, fileId)
	if err != nil || row == nil {
		i.log().Warn("compose: could not read the template file row", "error", err, "fileId", fileId)
		return ""
	}
	return stringOf(row["summary"])
}

// storeBytes writes the output to object storage. It returns the blobUrl
// and an EMPTY error string on success.
func (i *Integration) storeBytes(ctx context.Context, userId, fileId, name, mimeType string, data []byte) (string, string) {
	uploader, bucket := i.uploaderRef()
	objectName := fmt.Sprintf("library/%s/%s/%s", userId, fileId, name)
	if uploader == nil || bucket == "" {
		return "", "object storage is not configured on this node, so the bytes were not stored " +
			"(set MEMQL_AZURE_BLOB_CONTAINER and MEMQL_AZURE_STORAGE_CONNECTION_STRING)"
	}
	stored, err := uploader.Upload(ctx, bucket, objectName, data, mimeType)
	if err != nil {
		i.log().Error("compose: upload the materialized file", "error", err, "fileId", fileId)
		return "", "object storage refused the upload, so the bytes were not stored"
	}
	if strings.TrimSpace(stored) == "" {
		return objectName, ""
	}
	return stored, ""
}

// bumpRecipe records that a recipe ran. Never fatal: the file exists and
// a run count that did not move is a smaller wrong than reporting a
// successful materialization as failed.
func (i *Integration) bumpRecipe(ctx context.Context, recipeId string) {
	st := i.store()
	row, err := st.recipeById(ctx, recipeId)
	if err != nil || row == nil {
		i.log().Warn("compose: could not read the recipe to record its run", "error", err, "recipeId", recipeId)
		return
	}
	// core/num carries the ONE narrowing from a decoded payload number
	// to a Go int, in three NAMED answers -- a bare int(x) in a float64
	// or int64 arm is implementation-defined out of range and answers
	// with the integer indefinite value. The answer here is ZERO: an
	// absent or unreadable count means this recipe has produced nothing
	// we can account for, and starting again at 1 is the honest reading.
	next := runCountOf(row["runCount"]) + 1
	if err := st.recordRecipeRun(ctx, map[string]any{
		"recipeId":  recipeId,
		"lastRunAt": i.clock().UTC().Format(time.RFC3339),
		"runCount":  next,
	}); err != nil {
		i.log().Warn("compose: could not record the recipe run", "error", err, "recipeId", recipeId)
	}
}

// ---------------------------------------------------------------------------
// Naming and summaries
// ---------------------------------------------------------------------------

// outputFileName is the composition's name plus the format's extension,
// sanitised the way an upload's is: path separators and control
// characters removed, length bounded.
func outputFileName(name string, format pure.Format, deployableKind string) string {
	ext := format.Extension()
	if deployableKind != "" {
		ext = "zip"
	}
	stem := sanitiseFileStem(name)
	if stem == "" {
		stem = "composition"
	}
	if strings.HasSuffix(strings.ToLower(stem), "."+ext) {
		return stem
	}
	return stem + "." + ext
}

func sanitiseFileStem(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteByte('-')
		case r < 0x20 || r == 0x7F:
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(strings.TrimSpace(b.String()), ".")
	if len(out) > 120 {
		out = strings.TrimSpace(out[:120])
	}
	return out
}

// libraryFormatFor maps onto v1:library:file.format, the Library's own
// coarser classification. A package source zip is `other`, which renders
// the metadata-only card -- correct, because there is no viewer for a
// zip and pretending otherwise would offer a preview that fails.
func libraryFormatFor(f pure.Format, deployableKind string) string {
	if deployableKind != "" {
		return "other"
	}
	return f.LibraryFormat()
}

func fileSummary(a materializeArgs, p pure.Provenance) string {
	if a.DeployableKind != "" {
		return fmt.Sprintf("A %s package source, materialized from %s.", a.DeployableKind, p.SourceSummary())
	}
	return fmt.Sprintf("Materialized from %s.", p.SourceSummary())
}

func modelRows(models []pure.ModelContribution) []map[string]any {
	// EMPTY STAYS EMPTY AND IS NOT NIL-DROPPED. A composition that
	// reached no model has an empty list, and the call composer drops a
	// nil rather than writing `null` -- so an explicitly empty slice is
	// what records "nothing thought" instead of leaving the field
	// unwritten, which reads as "not recorded".
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"provider": m.Provider, "model": m.Model, "calls": m.Calls, "tokens": m.Tokens,
		})
	}
	return out
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// runCountOf narrows a decoded payload number to a Go int, through
// core/num's ZERO answer.
//
// A decoded JSON number arrives as float64, and a bare int(x) on one out
// of range is implementation-defined -- it answers with the integer
// indefinite value rather than saturating (core/num's header, memql#4779).
// Zero is the right answer here for two reasons: an absent count on a
// fresh recipe genuinely is zero, and an unreadable one means this
// recipe has produced nothing we can account for, so starting the count
// again is honest where a saturated maximum would not be.
func runCountOf(v any) int {
	switch t := v.(type) {
	case float64:
		return num.Float64OrZero(t)
	case int64:
		return num.Int64OrZero(t)
	case int:
		return t
	}
	return 0
}

func stringsOrNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
