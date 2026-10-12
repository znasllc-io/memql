package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	pure "github.com/znasllc-io/memql/component/compose"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
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
	OutputKind     string
	Sources        []SourceRef
	Draft          string
	SectionKeys    []string `json:",omitempty"`
	TemplateId     string
	FolderId       string
	AccountIds     []string
	DeployableKind string
	RecipeId       string
	Ceilings       map[string]any
	// RequestId is the caller's own name for this request. omitempty is
	// load-bearing: an in-run composition's id is derived from these arguments'
	// JSON, so a field that serialised as "" would re-key every step already in
	// flight.
	RequestId string `json:",omitempty"`
}

// blank is a request with nothing to compose from: no words, no draft, no
// sections, no sources, no template, recipe or special output.
func (a materializeArgs) blank() bool {
	return a.Statement == "" && strings.TrimSpace(a.Draft) == "" && len(a.SectionKeys) == 0 &&
		len(a.Sources) == 0 && a.TemplateId == "" && a.RecipeId == "" && a.OutputKind == "" && a.DeployableKind == ""
}

// describedMarkdown is a Markdown file asked for by DESCRIBING it: a statement,
// and nothing of the document's own to compose from -- no draft, sections,
// template, recipe or special output. Library files may come with it; they are
// references the revision workflow can read, and they are passed on with the
// description.
//
// Such a request is filed as the empty page with its statement kept on the
// composition. Writing the document is the Library's revision workflow, and the
// person's own client hands the description to it: a work run may not (see
// fromARun).
//
// A DATA SOURCE KEEPS THE COMPOSER. The revision workflow reads Markdown and
// images and cannot read the graph, so a request to write a document from
// records (a query, a concept row) has something of its own to compose from and
// only the composer can read it. Filing that request blank would throw the
// records away. The SDK's isDescribedMarkdown says the same thing; the two move
// together.
func (a materializeArgs) describedMarkdown() bool {
	return a.Format == pure.FormatMarkdown && a.Statement != "" && strings.TrimSpace(a.Draft) == "" &&
		len(a.SectionKeys) == 0 && a.TemplateId == "" && a.RecipeId == "" && a.OutputKind == "" && a.DeployableKind == "" &&
		onlyLibraryFiles(a.Sources)
}

// onlyLibraryFiles says every source is a Library file, or there are none.
func onlyLibraryFiles(sources []SourceRef) bool {
	for _, source := range sources {
		if source.Kind != KindLibraryFile {
			return false
		}
	}
	return true
}

// errFolderUnavailable is the refusal for a folder the caller cannot file into,
// and it is a sentence a person reads (the Materializer returns it as written),
// which is why it is capitalised and punctuated like one.
//
//nolint:staticcheck // ST1005: person-facing copy, not a log fragment.
var errFolderUnavailable = errors.New("That folder isn't available. Choose another folder.")

// requireUsableFolder refuses a folder the caller cannot file a new file into:
// one they cannot read (somebody else's, deleted, or no such id) or one that has
// been archived.
//
// IT IS A READ, NOT A TRUST. Creating the file stores whatever folderId it is
// given, so nothing downstream asks whether the caller can see that folder, and
// a file filed under one they cannot is a file they cannot find. An archived
// folder is one the person put in the Bin, and filing into it hides the file
// from every surface but the Bin. The read runs as the caller (a folder is
// theirs or it is not there) and fresh, so a folder archived a moment ago on
// another replica is not filed into on this node's cache.
func (i *Integration) requireUsableFolder(ctx context.Context, folderId string) error {
	folder, err := i.store().libraryFolderById(ctx, folderId)
	if err != nil {
		return fmt.Errorf("compose: checking the folder: %w", err)
	}
	if folder == nil || boolOf(folder["archived"]) {
		return errFolderUnavailable
	}
	return nil
}

// fromARun says a work run is making this call, and not a person's own client.
// It is the condition under which the Library refuses a revision request ("a
// person must prepare the document revision request", handleRequestDocumentRevision
// and the revision-item changes in integrations/library), so it is also the
// condition under which there is no client to hand a description on.
//
// RunFromContext answers true only for a context that names a run
// (RunContext.IsRun), which is the Library's own test; nothing is added to it
// here, so the two cannot disagree about what a run is.
//
// It is not `nested`, which materialize works out for a different question: a
// nested call is one whose composition BELONGS to the run, so it also needs the
// run's goal to attach to. A run that names no goal is still a run -- the
// Library still refuses it, and there is still no client -- while its
// composition opens a goal of its own like a person's.
func fromARun(ctx context.Context) bool {
	_, ok := common.RunFromContext(ctx)
	return ok
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
		OutputKind:     strings.TrimSpace(stringOf(args["outputKind"])),
		AccountIds:     stringList(args["accountIds"]),
		RequestId:      strings.TrimSpace(stringOf(args["requestId"])),
	}
	if raw, exists := args["sectionKeys"]; exists && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return out, err
		}
		if err := json.Unmarshal(encoded, &out.SectionKeys); err != nil || len(out.SectionKeys) == 0 {
			return out, fmt.Errorf("compose: sectionKeys must be a non-empty list of step keys")
		}
	}
	if out.Name == "" {
		return out, fmt.Errorf("compose: a materialization needs a name")
	}
	format, err := pure.ParseFormat(stringOf(args["format"]))
	if err != nil {
		return out, err
	}
	out.Format = format
	if out.Name, err = cleanFileName(out.Name, format); err != nil {
		return out, err
	}
	if out.OutputKind != "" && out.OutputKind != "email_template" {
		return out, errors.New("compose: unsupported outputKind")
	}
	if out.OutputKind == "email_template" && (format != pure.FormatJSON || out.DeployableKind != "") {
		return out, errors.New("compose: email templates require format json and no deployable kind")
	}
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

// cleanFileName turns what a person typed into the stem a file is named from.
//
// A FOLDER PATH IS REFUSED rather than flattened: a name is not a place to file
// into (folderId is), and silently rewriting "reports/Q3" to "reports-Q3" files
// something the person did not ask for. ONE trailing extension that already
// names the format is dropped, because the extension is added once, on filing:
// "Plan.md" stays Plan.md and is never Plan.md.md.
func cleanFileName(name string, format pure.Format) (string, error) {
	if strings.ContainsAny(name, "/\\") {
		return "", errors.New("compose: enter a file name without a folder path")
	}
	for _, ext := range formatNameExtensions(format) {
		suffix := "." + ext
		if len(name) >= len(suffix) && strings.EqualFold(name[len(name)-len(suffix):], suffix) {
			name = strings.TrimSpace(name[:len(name)-len(suffix)])
			break
		}
	}
	if name == "" {
		return "", errors.New("compose: a materialization needs a name")
	}
	return name, nil
}

// formatNameExtensions are the suffixes that say a name is already in a format:
// its file extension and, where that differs, its own name (md and markdown).
func formatNameExtensions(format pure.Format) []string {
	if ext := format.Extension(); ext != string(format) {
		return []string{ext, string(format)}
	}
	return []string{string(format)}
}

// executionRequest is captured once under the requesting actor. Retrying on
// another replica reads this snapshot rather than asking mutable sources again.
type executionRequest struct {
	Args         materializeArgs    `json:"args"`
	Resolved     []Resolved         `json:"resolved"`
	Started      time.Time          `json:"started"`
	Recipe       *pure.RenderRecipe `json:"recipe,omitempty"`
	TemplateBody string             `json:"templateBody,omitempty"`
	Sections     *sectionAssembly   `json:"sections,omitempty"`
	// Described says a person described this Markdown file and it is to be filed
	// as the empty page (see describedMarkdown). It is decided ONCE, when the
	// request is accepted, and saved with the request: the replica that executes
	// the run is usually not the one that accepted it, and only the accepting call
	// knows whether a person or a run made it. omitempty keeps every other saved
	// request exactly as it was.
	Described bool `json:"described,omitempty"`
}

var materializeIDEngine = id.NewUntracked()

func stableMaterializeID(parts ...string) string {
	return string(materializeIDEngine.MustFromMap(map[string]any{"parts": parts}))[:32]
}

func (i *Integration) materialize(ctx context.Context, userId, userEmail string, a materializeArgs) (map[string]any, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	if a.OutputKind != "" && (a.OutputKind != "email_template" || a.Format != pure.FormatJSON || a.DeployableKind != "") {
		return nil, errors.New("compose: email_template requires json format and no deployable kind")
	}
	if a.OutputKind == "email_template" {
		if len(a.AccountIds) != 1 || strings.TrimSpace(a.AccountIds[0]) == "" {
			return nil, errors.New("compose: choose one organization for this email template")
		}
		account, err := one(i.store().query(ctx, "query "+call("clientAccountById", map[string]any{"accountId": a.AccountIds[0]})))
		if err != nil {
			return nil, err
		}
		if account == nil || stringOf(account["status"]) != "active" {
			return nil, errors.New("compose: this organization is not active or accessible to you")
		}
	}
	if a.blank() {
		// REFUSED BEFORE ANYTHING IS WRITTEN. RenderBlank is the one place that
		// knows which formats have an empty document, so the request asks it
		// rather than repeating the list: a docx or pdf with nothing to put in it
		// would be a file that opens as an error, and no row is worth that.
		if _, err := pure.RenderBlank(a.Format, a.Name); err != nil {
			return nil, err
		}
	}
	rc, nested := common.RunFromContext(ctx)
	nested = nested && rc.RunId != "" && rc.GoalId != ""
	if err := validateSectionAssembly(a, rc, nested, userId); err != nil {
		return nil, err
	}
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
	} else if a.RequestId != "" {
		// A REPEAT OF THE SAME REQUEST IS THE SAME COMPOSITION. The id is derived
		// from the caller and the request id the way an in-run composition's is
		// derived from its run and step, so a double click (or a retry after a
		// dropped reply) finds the row the first call wrote instead of starting a
		// second file. The caller is part of the derivation: a request id is only
		// ever one person's, and another person's identical text must not find,
		// or collide with, their composition.
		//
		// THIS IS READ-THEN-WRITE, not a claim on the id: two identical requests
		// that arrive at the same moment on different replicas can both miss the
		// lookup and each open a goal. Callers are expected to dedupe in flight
		// (not send a second start while one is pending), and that is what keeps
		// the window shut in practice.
		compositionId = stableMaterializeID("request", memql.BareShortId(userId), a.RequestId)
		existing, err := i.store().compositionById(ctx, compositionId)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return map[string]any{
				"compositionId": compositionId, "goalId": stringOf(existing["goalId"]), "runId": stringOf(existing["runId"]),
				"format": stringOf(existing["format"]), "status": stringOf(existing["status"]),
			}, nil
		}
	}
	// THE FOLDER IS CHECKED HERE: after a repeat of an accepted request has had its
	// chance to find the composition it made (that file exists, and a folder
	// archived since is no reason to refuse the repeat), and before anything is
	// read or written for a new one.
	if a.FolderId != "" {
		if err := i.requireUsableFolder(ctx, a.FolderId); err != nil {
			return nil, err
		}
	}
	resolved, err := i.resolve(ctx, a.Sources)
	if err != nil {
		return nil, fmt.Errorf("compose: the sources could not be read: %w", err)
	}
	if err := i.captureSourceContents(ctx, resolved); err != nil {
		return nil, err
	}
	request := executionRequest{Args: a, Resolved: resolved, Started: i.clock().UTC()}
	// FILED BLANK ONLY WHEN A PERSON ASKED. The empty page is the first half of a
	// hand-off: the person's client reads the description off the composition and
	// gives it to the Library's revision workflow. The second half cannot be done
	// from inside a run -- the Library refuses a revision request made by one -- so
	// a call made BY a run (the planner's file delivery, an agent's composeFile:
	// both say "write this" in a statement) has no client to finish it, and filing
	// it blank would end the run with an empty document and nobody to write it.
	// Those keep the composer.
	request.Described = a.describedMarkdown() && !fromARun(ctx)
	if len(a.SectionKeys) > 0 {
		request.Sections, err = i.captureSections(ctx, rc, a.SectionKeys)
		if err != nil {
			return nil, err
		}
	}
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
		"outputKind": a.OutputKind,
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
	if stringOf(row["status"]) == "failed" {
		return nil, fmt.Errorf("%s: %s", terminalFailureCode, stringOf(row["failureReason"]))
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
	for _, key := range []string{"goalId", "runId", "outputFileId", "sourceFileId", "format", "name", "status", "sha256", "provenanceEmbedded", "provenanceNote", "modelsUsed", "deployableKind"} {
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
// Recoverable provider interruptions leave a resumable draft and keep their
// original error for the shared retry policy. Only terminal failures carry
// this code; the composition and work run must agree about recoverability.
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
	} else if row != nil && stringOf(row["runId"]) != "" {
		// The shared work loop owns retry budgets, backoff and stall detection.
		// Preserve this step's snapshot and identity when that loop can recover
		// from a provider/network interruption or a plan repair, including on another replica.
		_, terminal := workstate.TerminalFailureCode(cause.Error())
		symptom, _, classified := workstate.ClassifyByRules(workstate.Signal{ErrorMessage: cause.Error()})
		if !terminal && classified && (symptom == workstate.SymptomTransient || symptom == workstate.SymptomPlan) {
			status = "draft"
		}
	}
	if readErr != nil {
		i.log().Warn("compose: could not read terminal composition state", "error", readErr)
	}
	if err := i.store().updateCompositionState(cleanup, map[string]any{"compositionId": compositionId, "status": status, "failureReason": cause.Error()}); err != nil {
		i.log().Error("compose: could not record composition failure", "compositionId", compositionId, "error", err)
	}
	if status == "cancelled" || status == "draft" {
		// A CANCELLATION IS NOT THIS CODE. Somebody asked it to stop, which
		// the work spine already has a state for, and calling it a terminal
		// failure would report a person's own click back to them as a fault.
		// A resumable draft likewise retains the original error so the shared
		// classifier can apply the run's retry and escalation policy.
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
	// filedBlank: this request is written as its format's empty document -- because
	// there was nothing to compose it from (blank), or because a person described it
	// and their client hands the description on (described).
	filedBlank := a.blank() || request.Described
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

	var sourceFileId, fileId, fileName, mimeType string
	var needsSource, sourceComplete bool
	// blankDraft is set by composeBlankDraft and read by composeRenderOutput,
	// which stamps it on the recipe it saves: a request filed blank has no draft
	// to render, only its format's empty document.
	var blankDraft bool
	var templateName, templateBody string
	draft := pure.Draft{Title: a.Name, Body: a.Draft}
	var models []pure.ModelContribution
	if request.Sections != nil {
		draft.Body, models = request.Sections.Body, request.Sections.Models
	}
	var prov pure.Provenance
	var recipe pure.RenderRecipe
	var rendered pure.Result
	var output map[string]any
	var err error
	operations := map[string]workflowhost.Operation{
		"composeRecoverOutput": func(ctx context.Context, _ map[string]any) (any, error) {
			existingFileId := stableMaterializeID("materialized-file", compositionId)
			existingFile, err := st.libraryFileById(ctx, existingFileId)
			if err != nil {
				return fail("checking an earlier output failed", err)
			}
			sourceFileId = ""
			// A blank file has no recipe worth rebuilding, so it files no source package.
			needsSource = !filedBlank && ((a.Format != pure.FormatText && a.Format != pure.FormatMarkdown) || a.DeployableKind != "" || a.OutputKind != "")
			if needsSource {
				sourceFileId = stableMaterializeID("materialized-source", compositionId)
			}
			sourceComplete = !needsSource
			if needsSource {
				source, sourceErr := st.libraryFileById(ctx, sourceFileId)
				if sourceErr != nil {
					return fail("checking the source package failed", sourceErr)
				}
				sourceComplete = source != nil && stringOf(source["status"]) == "ready"
			}
			if existingFile != nil && sourceComplete {
				// The filing row is written only after uploading all bytes. Its identity is
				// stable even when the final composition update was interrupted.
				if err := st.setLibraryFileReady(ctx, existingFileId, stringOf(existingFile["summary"])); err != nil {
					return fail("the output file could not be marked ready", err)
				}
				if err := st.updateCompositionState(ctx, map[string]any{"compositionId": compositionId, "status": "ready", "failureReason": "", "outputFileId": existingFileId, "sourceFileId": sourceFileId, "sha256": existingFile["sha256"]}); err != nil {
					return nil, err
				}
				row, err := st.compositionById(ctx, compositionId)
				if err != nil {
					return nil, err
				}
				output = compositionResult(compositionId, row)
				return true, nil
			}

			return false, nil
		},
		"composeReadTemplate": func(ctx context.Context, _ map[string]any) (any, error) {
			if a.TemplateId != "" && request.Recipe == nil {
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
					templateBody, terr = i.templateBody(ctx, fileId)
					if terr != nil {
						return fail("the template contents could not be read: "+terr.Error(), terr)
					}
				}
			}

			if err := st.updateCompositionState(ctx, map[string]any{
				"compositionId": compositionId, "status": "composing", "failureReason": "", "runId": runId,
			}); err != nil {
				i.log().Warn("compose: could not mark the composition composing", "error", err, "compositionId", compositionId)
			}

			return nil, nil
		},
		"composeWorkflowFacts": func(context.Context, map[string]any) (any, error) {
			// isBlank is false once a recipe snapshot exists, and that is safe: the
			// snapshot a blank or described request saves is marked Blank (see
			// composeRenderOutput), so a resumed run takes the restore branch and still
			// renders the empty document rather than the draft it restored.
			return map[string]any{"savedRecipe": request.Recipe != nil, "hasSections": request.Sections != nil, "hasComposer": i.composerRef() != nil, "hasDraft": strings.TrimSpace(draft.Body) != "", "hasRows": len(draft.Rows) > 0, "format": string(a.Format), "outputKind": a.OutputKind, "needsSource": needsSource, "sourceComplete": sourceComplete, "hasRecipeId": a.RecipeId != "", "isBlank": filedBlank && request.Recipe == nil}, nil
		},
		"composeRestoreDraft": func(context.Context, map[string]any) (any, error) {
			if request.Recipe == nil {
				return nil, errors.New("compose: no saved recipe")
			}
			draft, models = request.Recipe.Draft, request.Recipe.Provenance.Models
			templateBody = request.TemplateBody
			templateName = request.Recipe.Provenance.TemplateName

			return nil, nil
		},
		"composeGenerateDraft": func(ctx context.Context, _ map[string]any) (any, error) {
			if request.Sections != nil {
				return nil, errors.New("compose: completed sections must be rendered without regeneration")
			}
			if i.composerRef() == nil {
				return nil, errors.New("compose: no composer configured")
			}
			reply, cerr := i.composerRef().Compose(ctx, ComposeRequest{
				Statement:    a.Statement,
				Format:       a.Format,
				OutputKind:   a.OutputKind,
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
			return nil, nil
		},
		"composeRequireDraft": func(ctx context.Context, _ map[string]any) (any, error) {
			if strings.TrimSpace(draft.Body) == "" {
				return fail("this node has no composer configured and no draft was supplied, so there is nothing to render", nil)
			}
			return nil, nil
		},
		"composeBlankDraft": func(context.Context, map[string]any) (any, error) {
			blankDraft = true
			return nil, nil
		},
		"composeUseSourceRows": func(context.Context, map[string]any) (any, error) {
			draft.Header, draft.Rows = tabularRows(resolved)
			return nil, nil
		},
		"composeRenderOutput": func(ctx context.Context, _ map[string]any) (any, error) {
			if err := st.updateCompositionState(ctx, map[string]any{
				"compositionId": compositionId, "status": "rendering",
			}); err != nil {
				i.log().Warn("compose: could not mark the composition rendering", "error", err, "compositionId", compositionId)
			}

			if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
				return fail("materialization stopped", err)
			}

			// --- steps 3 + 4: render and stamp ---
			prov = pure.Provenance{
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
			if request.Sections != nil {
				prov.Sources = append(prov.Sources, request.Sections.Sources...)
			}

			if a.OutputKind == "email_template" && request.Recipe == nil {
				draft.Body, err = EmbedEmailAssets(draft.Body, resolved)
				if err != nil {
					return fail("embedding email assets failed", err)
				}
			}
			// Blank is stamped on the recipe, not kept in blankDraft alone: the saved
			// snapshot is all a resumed run on another replica restores, and it must
			// come back rendering the empty document rather than an ordinary one.
			recipe = pure.RenderRecipe{Name: a.Name, Format: a.Format, OutputKind: a.OutputKind, DeployableKind: a.DeployableKind, Draft: draft, Provenance: prov, Blank: blankDraft}
			if request.Recipe != nil {
				recipe = *request.Recipe
			} else {
				request.Recipe, request.TemplateBody = &recipe, templateBody
				raw, marshalErr := json.Marshal(request)
				if marshalErr != nil {
					return fail("capturing render recipe failed", marshalErr)
				}
				var snapshot map[string]any
				if err = json.Unmarshal(raw, &snapshot); err != nil {
					return fail("capturing render recipe failed", err)
				}
				if err = st.writeInternal(ctx, "mutation "+call("saveCompositionRecipe", map[string]any{"compositionId": compositionId, "request": snapshot})); err != nil {
					return fail("saving render recipe failed", err)
				}
			}
			rendered, err = pure.RenderRecipeBytes(recipe)
			if err != nil {
				return fail("rendering the file failed: "+err.Error(), err)
			}

			if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
				return fail("materialization stopped", err)
			}
			fileId = stableMaterializeID("materialized-file", compositionId)
			fileName = outputFileName(a.Name, a.Format, a.DeployableKind)
			if a.OutputKind == "email_template" {
				fileName = strings.TrimSuffix(fileName, ".json") + ".email.json"
			}
			mimeType = a.Format.MimeType()
			if a.DeployableKind != "" {
				mimeType = "application/zip"
			}

			return nil, nil
		},
		"composeFileSource": func(ctx context.Context, _ map[string]any) (any, error) {
			inputs, marshalErr := json.MarshalIndent(i.narrowed(resolved), "", "  ")
			if marshalErr != nil {
				return fail("capturing source inputs failed", marshalErr)
			}
			extras := map[string][]byte{"inputs/sources.json": inputs, "inputs/template.txt": []byte(templateBody)}
			for _, source := range resolved {
				for _, asset := range source.Files {
					if len(asset.Image) > 0 {
						extras["inputs/assets/"+asset.SHA256] = asset.Image
					}
				}
			}
			source, packageErr := pure.BuildSourcePackage(recipe, fileName, rendered, extras)
			if packageErr != nil {
				return fail("source package verification failed", packageErr)
			}
			sourceName := strings.TrimSuffix(fileName, "."+string(a.Format)) + "-source.zip"
			blob, storageError := i.storeBytes(ctx, userId, sourceFileId, sourceName, "application/zip", source.Bytes)
			if storageError != "" {
				return fail(storageError, nil)
			}
			rc, _ := common.RunFromContext(ctx)
			if err = st.createLibraryFile(ctx, map[string]any{"fileId": sourceFileId, "name": sourceName, "mimeType": "application/zip", "size": len(source.Bytes), "sha256": source.SHA256(), "blobUrl": blob, "source": "agent_generated", "producedByRunId": runId, "producedByStepKey": rc.StepKey, "format": "other", "summary": source.Note, "folderId": a.FolderId}); err != nil {
				return fail("filing source package failed", err)
			}
			if err = st.setLibraryFileReady(ctx, sourceFileId, source.Note); err != nil {
				return fail("completing source package failed", err)
			}
			return nil, nil
		},
		"composeFileOutput": func(ctx context.Context, _ map[string]any) (any, error) {
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
			// The recipe, not blankDraft, says whether this file is blank: a resumed
			// pass restores the saved recipe and never takes the blank branch itself.
			summary := fileSummary(a, prov, recipe.Blank)
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
				"summary":           summary,
				"folderId":          a.FolderId,
			}); err != nil {
				return fail("the output could not be filed in your Library: "+err.Error(), err)
			}
			if err := st.setLibraryFileReady(ctx, fileId, summary); err != nil {
				// The native work step completes only after the durable delivery
				// receipt exists. A stored blob alone cannot mark the run successful.
				return fail("the output file could not be marked ready", err)
			}

			if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
				return fail("materialization stopped", err)
			}

			return nil, nil
		},
		"composeCompleteOutput": func(ctx context.Context, _ map[string]any) (any, error) {
			if err := st.updateCompositionState(ctx, map[string]any{
				"compositionId":      compositionId,
				"status":             "ready",
				"outputFileId":       fileId,
				"sourceFileId":       sourceFileId,
				"modelsUsed":         modelRows(models),
				"provenanceEmbedded": rendered.Embedded,
				"provenanceNote":     rendered.Note,
				"sha256":             rendered.SHA256(),
			}); err != nil {
				return nil, fmt.Errorf("compose: the file was written but the record could not be completed: %w", err)
			}
			output = map[string]any{
				"compositionId":      compositionId,
				"goalId":             goalId,
				"runId":              runId,
				"outputFileId":       fileId,
				"sourceFileId":       sourceFileId,
				"name":               fileName,
				"format":             string(a.Format),
				"deployableKind":     a.DeployableKind,
				"sizeBytes":          len(rendered.Bytes),
				"sha256":             rendered.SHA256(),
				"provenanceEmbedded": rendered.Embedded,
				"provenanceNote":     rendered.Note,
				"modelsUsed":         modelRows(models),
				"sourcesResolved":    len(resolved),
			}
			return nil, nil
		},
		"composeRecordRecipeUse": func(ctx context.Context, _ map[string]any) (any, error) {
			i.bumpRecipe(ctx, a.RecipeId)
			return nil, nil
		},
	}
	// The native scope rechecks cancellation at every effect boundary.
	for name, op := range operations {
		operations[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if err := i.checkCompositionCancellation(ctx, compositionId); err != nil {
				return fail("materialization stopped", err)
			}
			return op(ctx, args)
		}
	}
	_, err = workflowhost.Run(ctx, "composeMaterializeWorkflow", nil, workflowhost.Options{Logger: i.log(), Operations: operations})
	return output, err
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

// The source package includes the template actually used, not its summary.
// An unavailable template cannot produce a success claiming to have used it.
func (i *Integration) templateBody(ctx context.Context, fileId string) (string, error) {
	row, err := i.store().libraryFileById(ctx, fileId)
	if err != nil || row == nil {
		return "", fmt.Errorf("template file is unavailable: %v", err)
	}
	reader := i.sourceDownloader()
	if reader == nil || stringOf(row["blobUrl"]) == "" {
		return "", errors.New("template storage is unavailable")
	}
	data, err := reader.DownloadURLWithLimit(ctx, stringOf(row["blobUrl"]), maxReferenceText)
	if err != nil {
		return "", err
	}
	if digest := stringOf(row["sha256"]); digest != "" && digest != (pure.Result{Bytes: data}).SHA256() {
		return "", errors.New("template bytes do not match their saved digest")
	}
	content, err := readReferenceMember(stringOf(row["name"]), data, &referenceBudget{})
	if err != nil {
		return "", err
	}
	if content.Text == "" {
		return "", errors.New("use a text, Markdown, HTML or structured text template")
	}
	return content.Text, nil
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

// fileSummary is the one line the Library lists a file by. A file filed blank
// says so: "Materialized from no sources" reads as a fault on a file that was
// never going to be made from any.
func fileSummary(a materializeArgs, p pure.Provenance, blank bool) string {
	if blank {
		return "Created blank."
	}
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
