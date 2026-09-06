package compose

import (
	"context"
	"fmt"
	"strings"
	"time"

	pure "github.com/znasllc-io/memql/component/compose"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// materialize.go -- the five steps, in order.
//
//	gather      deterministic   resolve every source ref to rows/bytes
//	compose     REASONING       the one model call: rows + template -> the draft
//	render      deterministic   draft + template -> the format's bytes
//	stamp       deterministic   embed provenance, per the format's table
//	file        deterministic   write v1:library:file + the composition record
//
// FOUR OF THE FIVE REACH NO MODEL, and that split is the whole product
// claim rather than an implementation detail: the second quarter's
// report costs a page read and a render. The one that does is `compose`,
// and on a catalog hit through the work spine it does not either.
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

// materialize runs the five steps and returns what the caller sees.
//
// THE ROW IS OPENED FIRST AND UPDATED AS IT GOES, never written once at
// the end. A composition that failed halfway with no row is one the
// person cannot find, ask about or retry -- and "it just did nothing" is
// the report that follows. Every failure below marks the row `failed`
// with a reason before returning the error.
func (i *Integration) materialize(ctx context.Context, userId, userEmail string, a materializeArgs) (map[string]any, error) {
	st := i.store()
	compositionId := id.NewShortId()
	started := i.clock().UTC()

	// --- the goal, first, so the composition can name it ---
	//
	// EVERY MATERIALIZATION IS A GOAL (design D6), opened through the work
	// spine's OWN `createGoal` builtin over this package's engine handle,
	// under the caller's own actor. That is deliberately a DSL call rather
	// than a Go seam onto integrations/work: `createGoal` exists there as
	// a capability handler and not as an exported method, and adding one
	// would couple two integrations in Go for something the DSL already
	// exposes -- plus give `requestedVia` a second spelling.
	//
	// A FAILURE HERE IS LOGGED AND NOT FATAL. The file is the deliverable
	// and the tracking is around it, so a node with no work plug-in -- or
	// a work spine having a bad day -- still materializes. The composition
	// then carries an empty goalId, which the app renders as "not tracked"
	// rather than as a broken link.
	goalId, runId := i.openGoal(ctx, compositionId, a)

	// --- step 1: gather ---
	//
	// BEFORE THE ROW, and that ordering is deliberate. Resolving is a
	// set of READS under the caller's own actor: it cannot fail as a
	// whole (a source that finds nothing records its own problem and the
	// others carry on), and doing it first is what lets the row record
	// what was ACTUALLY found, with each source's capturedAt. A row
	// written first would have to be updated with its own sources a
	// moment later, and `sources` is not a field updateCompositionState
	// accepts -- deliberately, because what a composition was made from
	// must not be rewritable after the fact.
	resolved, err := i.resolve(ctx, a.Sources)
	if err != nil {
		return nil, fmt.Errorf("compose: the sources could not be read: %w", err)
	}

	// --- the row, before anything that CAN fail ---
	if err := st.createComposition(ctx, map[string]any{
		"compositionId":  compositionId,
		"name":           a.Name,
		"statement":      a.Statement,
		"format":         string(a.Format),
		"sources":        rowSources(resolved),
		"templateId":     a.TemplateId,
		"folderId":       a.FolderId,
		"accountIds":     stringsOrNil(a.AccountIds),
		"goalId":         goalId,
		"runId":          runId,
		"recipeId":       a.RecipeId,
		"deployableKind": a.DeployableKind,
	}); err != nil {
		return nil, fmt.Errorf("compose: opening the composition record: %w", err)
	}

	fail := func(reason string, err error) (map[string]any, error) {
		if uerr := st.updateCompositionState(ctx, map[string]any{
			"compositionId": compositionId,
			"status":        "failed",
			"failureReason": reason,
		}); uerr != nil {
			i.log().Error("compose: could not record a failure on the composition", "error", uerr, "compositionId", compositionId)
		}
		if err == nil {
			err = fmt.Errorf("compose: %s", reason)
		}
		return nil, err
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

	// --- step 5: file ---
	fileId := id.NewShortId()
	fileName := outputFileName(a.Name, a.Format, a.DeployableKind)
	mimeType := a.Format.MimeType()
	if a.DeployableKind != "" {
		mimeType = "application/zip"
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

	if err := st.createLibraryFile(ctx, map[string]any{
		"fileId":   fileId,
		"name":     fileName,
		"mimeType": mimeType,
		"size":     len(rendered.Bytes),
		"sha256":   rendered.SHA256(),
		"blobUrl":  blobUrl,
		"source":   "agent_generated",
		"format":   libraryFormatFor(a.Format, a.DeployableKind),
		"summary":  fileSummary(a, prov),
		"folderId": a.FolderId,
	}); err != nil {
		return fail("the output could not be filed in your Library: "+err.Error(), err)
	}
	if err := st.setLibraryFileReady(ctx, fileId, fileSummary(a, prov)); err != nil {
		// NOT FATAL. The bytes are stored and the row exists; a status
		// that stayed at `stored` costs the file its "ready" mark and
		// nothing else, and failing here would report a materialization
		// that plainly worked as broken.
		i.log().Warn("compose: could not mark the output file ready", "error", err, "fileId", fileId)
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

// openGoal opens the v1:work:goal this materialization is, through the
// work spine's own `createGoal` builtin.
//
// UNSTAMPED, so the caller's actor decides: the goal is the caller's own,
// `createGoal` is `@sdk` rather than `@serverOnly`, and stamping internal
// origin here would widen a call that needs no widening.
//
// It returns EMPTY IDS on every failure rather than an error, and the
// caller carries on. A work spine that is absent, refusing or slow must
// not be able to stop somebody making a file; what it costs is the Nexus
// hand-off and replay, which the app says plainly rather than pretending.
func (i *Integration) openGoal(ctx context.Context, compositionId string, a materializeArgs) (string, string) {
	statement := strings.TrimSpace(a.Statement)
	if statement == "" {
		statement = "Materialize " + a.Name + " as " + string(a.Format)
	}
	args := map[string]any{
		"statement":    statement,
		"requestedVia": "materializer",
		"input": map[string]any{
			"compositionId": compositionId,
			"format":        string(a.Format),
			"templateId":    a.TemplateId,
		},
	}
	if len(a.AccountIds) > 0 {
		args["accountIds"] = a.AccountIds
	}
	if len(a.Ceilings) > 0 {
		args["ceilings"] = a.Ceilings
	}
	rows, err := i.store().query(ctx, "builtin "+call("createGoal", args))
	if err != nil {
		i.log().Warn("compose: could not open the goal for a materialization; it will not appear in Nexus",
			"error", err, "compositionId", compositionId)
		return "", ""
	}
	if len(rows) == 0 {
		return "", ""
	}
	return stringOf(rows[0]["goalId"]), stringOf(rows[0]["runId"])
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
