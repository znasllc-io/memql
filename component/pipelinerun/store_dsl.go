package pipelinerun

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/num"
)

// store_dsl.go -- the production Store, over dsl/pipelines' constructs.
//
// EVERY CONSTRUCT NAME THIS PACKAGE CALLS IS BELOW, and nowhere else: a
// rename in dsl/pipelines is one edit here, and a test renders every call and
// hands it to the real parser.
const (
	// Person-facing reads, owner-scoped by their own filters.
	qPipelinesForOwner    = "pipelinesForOwner"    // ()
	qPipelineForOwner     = "pipelineForOwner"     // (pipelineId)
	qPipelineForPackage   = "pipelineForPackage"   // (packageId)
	qPipelineRunsForOwner = "pipelineRunsForOwner" // (pipelineId?)
	qPipelineRunForOwner  = "pipelineRunForOwner"  // (runId)

	// Server-only reads, every owner's rows, cluster-owner conjunct.
	qPipelinesForRepository     = "pipelinesForRepository"     // (repository)
	qPipelinesPolled            = "pipelinesPolled"            // ()
	qPipelineByID               = "pipelineById"               // (pipelineId)
	qPipelineRunsForKey         = "pipelineRunsForKey"         // (runKey)
	qPipelineRunsForPipelineSha = "pipelineRunsForPipelineSha" // (pipelineId, sha)
	qPipelineRunByCheckRun      = "pipelineRunByCheckRun"      // (repository, checkRunId)
	qPipelineRunsUnfinished     = "pipelineRunsUnfinished"     // ()
	qPipelineRunByID            = "pipelineRunById"            // (runId)

	// Writes, every one @serverOnly.
	mCreatePipeline    = "createPipeline"
	mUpdatePipeline    = "updatePipeline"
	mCreatePipelineRun = "createPipelineRun"
	mUpdatePipelineRun = "updatePipelineRun"

	// Deployables' read of the source a pipeline hangs off (dsl/platform).
	qPackageByID = "packageById" // (packageId)
)

// systemActorName is who the server-only reads are made as:
// auth.SystemActor("pipelines"), a synthetic cluster owner -- which is what
// the reads' `actor.isClusterOwner == true` conjunct tests -- that can never
// own a row.
const systemActorName = "pipelines"

// NewDSLStore is the production Store over engine.
func NewDSLStore(engine Engine) Store { return &dslStore{engine: engine} }

type dslStore struct {
	engine Engine
}

// ---------------------------------------------------------------------------
// The three authorities
// ---------------------------------------------------------------------------

// callerRead runs a person-facing read under whatever actor ctx carries --
// the caller's. Unstamped: these constructs are not @serverOnly, and the
// owner conjunct in their filters is what decides the rows.
func (s *dslStore) callerRead(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, errNoStore
	}
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pipelines: %s: %w", constructOf(query), err)
	}
	return rowsOf(res), nil
}

// systemRead runs a server-only read as this package's own system actor. It
// REPLACES whatever actor ctx carried: a delivery, a schedule or a driver has
// no person behind it, and a person-facing caller that reaches one of these
// has already been judged by an owner-scoped read of its own.
func (s *dslStore) systemRead(ctx context.Context, query string) ([]map[string]any, error) {
	return s.executeInternal(auth.ContextWithSystemActor(ctx, systemActorName), query)
}

// ownerWrite runs a write under owner's borrowed authority. The mutations
// stamp ownerUserId from the actor, so the actor must BE the owner -- and an
// empty owner is refused here, before anything is written: auth's helper
// leaves ctx untouched for a blank id, which would write the row under
// whichever actor the caller happened to carry.
func (s *dslStore) ownerWrite(ctx context.Context, owner, query string) error {
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("pipelines: %s: the row's owner is unknown, and a pipelines row is written only under its owner's authority", constructOf(query))
	}
	_, err := s.executeInternal(auth.ContextWithUserActor(ctx, strings.TrimSpace(owner)), query)
	return err
}

// executeInternal is THE ONE PLACE this package stamps internal origin, and
// it stamps INLINE, as the argument to the one Execute that needs it, so the
// mark dies at that call and no later frame inherits it (memql#2879,
// memql#2989; internal_origin_test.go counts this site). Every server-only
// read and every write funnels through here; the actor was chosen by the
// caller above, which is the whole of what the stamp does NOT decide.
func (s *dslStore) executeInternal(actorCtx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, errNoStore
	}
	res, err := s.engine.Execute(auth.ContextWithInternalOrigin(actorCtx), query)
	if err != nil {
		return nil, fmt.Errorf("pipelines: %s: %w", constructOf(query), err)
	}
	return rowsOf(res), nil
}

// ---------------------------------------------------------------------------
// Person-facing reads
// ---------------------------------------------------------------------------

func (s *dslStore) PackageForCaller(ctx context.Context, packageID string) (*PackageSource, error) {
	rows, err := s.callerRead(ctx, call("query", qPackageByID, argString("packageId", packageID)))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	p := packageFromRow(rows[0])
	return &p, nil
}

func (s *dslStore) PipelineForOwner(ctx context.Context, pipelineID string) (*Pipeline, error) {
	return onePipeline(s.callerRead(ctx, call("query", qPipelineForOwner, argString("pipelineId", bareID(pipelineID)))))
}

func (s *dslStore) PipelineForPackage(ctx context.Context, packageID string) (*Pipeline, error) {
	return onePipeline(s.callerRead(ctx, call("query", qPipelineForPackage, argString("packageId", bareID(packageID)))))
}

func (s *dslStore) PipelinesForOwner(ctx context.Context) ([]Pipeline, error) {
	return allPipelines(s.callerRead(ctx, call("query", qPipelinesForOwner)))
}

func (s *dslStore) RunForOwner(ctx context.Context, runID string) (*Run, error) {
	return oneRun(s.callerRead(ctx, call("query", qPipelineRunForOwner, argString("runId", bareID(runID)))))
}

func (s *dslStore) RunsForOwner(ctx context.Context, pipelineID string) ([]Run, error) {
	// The pipeline is optional: absent lists every pipeline's runs.
	return allRuns(s.callerRead(ctx, call("query", qPipelineRunsForOwner, argStringIfSet("pipelineId", bareID(pipelineID)))))
}

// ---------------------------------------------------------------------------
// Server-only reads
// ---------------------------------------------------------------------------

func (s *dslStore) PipelinesForRepository(ctx context.Context, repository string) ([]Pipeline, error) {
	return allPipelines(s.systemRead(ctx, call("query", qPipelinesForRepository,
		argString("repository", normalizeRepository(repository)))))
}

func (s *dslStore) PipelinesPolled(ctx context.Context) ([]Pipeline, error) {
	return allPipelines(s.systemRead(ctx, call("query", qPipelinesPolled)))
}

func (s *dslStore) PipelineByID(ctx context.Context, pipelineID string) (*Pipeline, error) {
	return onePipeline(s.systemRead(ctx, call("query", qPipelineByID, argString("pipelineId", bareID(pipelineID)))))
}

func (s *dslStore) RunsForKey(ctx context.Context, runKey string) ([]Run, error) {
	return allRuns(s.systemRead(ctx, call("query", qPipelineRunsForKey, argString("runKey", runKey))))
}

func (s *dslStore) RunsForPipelineSHA(ctx context.Context, pipelineID, sha string) ([]Run, error) {
	return allRuns(s.systemRead(ctx, call("query", qPipelineRunsForPipelineSha,
		argString("pipelineId", bareID(pipelineID)),
		argString("sha", strings.ToLower(strings.TrimSpace(sha))))))
}

func (s *dslStore) RunByCheckRun(ctx context.Context, repository string, checkRunID int64) (*Run, error) {
	if checkRunID <= 0 {
		return nil, nil
	}
	return oneRun(s.systemRead(ctx, call("query", qPipelineRunByCheckRun,
		argString("repository", normalizeRepository(repository)),
		argString("checkRunId", strconv.FormatInt(checkRunID, 10)))))
}

func (s *dslStore) RunsUnfinished(ctx context.Context) ([]Run, error) {
	return allRuns(s.systemRead(ctx, call("query", qPipelineRunsUnfinished)))
}

func (s *dslStore) RunByID(ctx context.Context, runID string) (*Run, error) {
	return oneRun(s.systemRead(ctx, call("query", qPipelineRunByID, argString("runId", bareID(runID)))))
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// CreatePipeline connects a pipeline, or reconnects it: createPipeline is a
// read-merge insert at the derived id, so a second call restates the
// configuration, marks the row active and moves connectedAt, while the
// poll's heads and the timing table survive. The allowlist is ALWAYS sent,
// empty included: it is the owner's answer at connect, and omitting an empty
// one would keep the previous connection's.
func (s *dslStore) CreatePipeline(ctx context.Context, p Pipeline) error {
	compute := p.Compute
	if compute == "" {
		compute = pipelines.ComputeCluster
	}
	return s.ownerWrite(ctx, p.OwnerUserID, call("mutation", mCreatePipeline,
		argString("pipelineId", bareID(p.ID)),
		argString("packageId", bareID(p.PackageID)),
		argStringIfSet("accountId", bareID(p.AccountID)),
		argString("name", p.Name),
		argString("repository", normalizeRepository(p.Repository)),
		argStringIfSet("defaultBranch", p.DefaultBranch),
		argString("installationId", formatID(p.InstallationID)),
		argString("credentialId", bareID(p.CredentialID)),
		argString("delivery", p.Delivery),
		argString("compute", string(compute)),
		argValue("secretNames", stringList(p.SecretNames)),
		argValueIf(len(p.ChannelIDs) > 0, "channelIds", stringList(p.ChannelIDs)),
	))
}

// UpdatePipeline writes the named fields of patch and nothing else.
func (s *dslStore) UpdatePipeline(ctx context.Context, owner, pipelineID string, patch PipelinePatch) error {
	args := []string{argString("pipelineId", bareID(pipelineID))}
	if patch.Name != nil {
		args = append(args, argString("name", *patch.Name))
	}
	if patch.DefaultBranch != nil {
		args = append(args, argString("defaultBranch", *patch.DefaultBranch))
	}
	if patch.Delivery != nil {
		args = append(args, argString("delivery", *patch.Delivery))
	}
	if patch.Compute != nil {
		args = append(args, argString("compute", string(*patch.Compute)))
	}
	if patch.Status != nil {
		args = append(args, argString("status", *patch.Status))
	}
	if patch.SecretNames != nil {
		args = append(args, argValue("secretNames", stringList(*patch.SecretNames)))
	}
	if patch.ChannelIDs != nil {
		args = append(args, argValue("channelIds", stringList(*patch.ChannelIDs)))
	}
	if patch.Heads != nil {
		args = append(args, argValue("heads", stringMap(*patch.Heads)))
	}
	if patch.Timings != nil {
		args = append(args, argValue("timings", floatMap(*patch.Timings)))
	}
	if patch.TimingsRunID != nil {
		args = append(args, argString("timingsRunId", bareID(*patch.TimingsRunID)))
	}
	if patch.TimingsUpdatedAt != nil {
		args = append(args, argString("timingsUpdatedAt", formatTime(*patch.TimingsUpdatedAt)))
	}
	return s.ownerWrite(ctx, owner, call("mutation", mUpdatePipeline, args...))
}

// CreateRun opens a run with every open-time field. An empty optional field
// is OMITTED rather than written empty: the row is new, so absent is the
// truth, and the mutation stamps conclusion and checkRunState empty itself.
func (s *dslStore) CreateRun(ctx context.Context, r Run) error {
	queued := r.QueuedAt
	if queued.IsZero() {
		queued = time.Now().UTC()
	}
	return s.ownerWrite(ctx, r.OwnerUserID, call("mutation", mCreatePipelineRun,
		argString("runId", bareID(r.ID)),
		argString("pipelineId", bareID(r.PipelineID)),
		argStringIfSet("accountId", bareID(r.AccountID)),
		argString("repository", normalizeRepository(r.Repository)),
		argString("sha", strings.ToLower(strings.TrimSpace(r.SHA))),
		argString("mode", string(r.Mode)),
		argString("event", string(r.Event)),
		argString("runKey", r.RunKey),
		argInt("attempt", int64(r.Attempt)),
		argString("trigger", r.Trigger),
		argStringIfSet("rerunOf", bareID(r.RerunOf)),
		argStringIfSet("deliveryId", r.DeliveryID),
		argIntIfSet("pullRequest", int64(r.PullRequest)),
		argStringIfSet("headBranch", r.HeadBranch),
		argStringIfSet("baseSha", strings.ToLower(strings.TrimSpace(r.BaseSHA))),
		argStringIfSet("title", r.Title),
		argStringIfSet("version", r.Version),
		argString("status", r.Status),
		argStringIfSet("conclusion", r.Conclusion),
		argStringIfSet("refusalCode", r.RefusalCode),
		argStringIfSet("refusalMessage", r.RefusalMessage),
		argStringIfSet("refusalScope", r.RefusalScope),
		argStringIfSet("checkRunId", formatIDIfSet(r.CheckRunID)),
		argStringIfSet("checkRunState", r.CheckRunState),
		argValueIf(len(r.Notes) > 0, "notes", noteList(r.Notes)),
		argString("queuedAt", formatTime(queued)),
		argStringIfSet("finishedAt", formatTimeIfSet(r.FinishedAt)),
		argIntIfSet("durationMs", r.DurationMs),
	))
}

// UpdateRun writes the named fields of patch and nothing else.
func (s *dslStore) UpdateRun(ctx context.Context, owner, runID string, patch RunPatch) error {
	args := []string{argString("runId", bareID(runID))}
	str := func(name string, v *string) {
		if v != nil {
			args = append(args, argString(name, *v))
		}
	}
	at := func(name string, v *time.Time) {
		if v != nil {
			args = append(args, argString(name, formatTimeIfSet(*v)))
		}
	}
	str("status", patch.Status)
	str("conclusion", patch.Conclusion)
	str("refusalCode", patch.RefusalCode)
	str("refusalMessage", patch.RefusalMessage)
	str("refusalScope", patch.RefusalScope)
	if patch.CheckRunID != nil {
		args = append(args, argString("checkRunId", formatIDIfSet(*patch.CheckRunID)))
	}
	str("checkRunState", patch.CheckRunState)
	if patch.Notes != nil {
		args = append(args, argValue("notes", noteList(*patch.Notes)))
	}
	str("workRunId", patch.WorkRunID)
	if patch.WorkGoalID != nil {
		args = append(args, argString("workGoalId", bareID(*patch.WorkGoalID)))
	}
	str("driverNodeId", patch.DriverNodeID)
	at("driverHeartbeatAt", patch.DriverHeartbeatAt)
	if patch.CancelRequested != nil {
		args = append(args, argValue("cancelRequested", *patch.CancelRequested))
	}
	str("cancelledBy", patch.CancelledBy)
	if patch.Stages != nil {
		args = append(args, argValue("stages", stageList(*patch.Stages)))
	}
	at("startedAt", patch.StartedAt)
	at("finishedAt", patch.FinishedAt)
	if patch.DurationMs != nil {
		args = append(args, argInt("durationMs", *patch.DurationMs))
	}
	return s.ownerWrite(ctx, owner, call("mutation", mUpdatePipelineRun, args...))
}

// ---------------------------------------------------------------------------
// Rendering a call
// ---------------------------------------------------------------------------
//
// A Go caller's only write channel is MemQL TEXT, so every value is rendered
// as a literal: strings through langparser.QuoteString (never %q, whose
// escapes the lexer refuses), objects with every key quoted and sorted, lists
// and numbers in the one spelling the parser reads. A blank rendered argument
// is dropped, which is how an optional one is omitted.

// call renders "<kind> <name>(<args>)", dropping blank arguments.
func call(kind, name string, args ...string) string {
	kept := make([]string, 0, len(args))
	for _, a := range args {
		if a != "" {
			kept = append(kept, a)
		}
	}
	return kind + " " + name + "(" + strings.Join(kept, ", ") + ")"
}

func argString(name, value string) string { return name + ": " + langparser.QuoteString(value) }

func argStringIfSet(name, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return argString(name, value)
}

func argInt(name string, value int64) string { return name + ": " + strconv.FormatInt(value, 10) }

func argIntIfSet(name string, value int64) string {
	if value == 0 {
		return ""
	}
	return argInt(name, value)
}

func argValue(name string, value any) string { return name + ": " + literal(value) }

func argValueIf(ok bool, name string, value any) string {
	if !ok {
		return ""
	}
	return argValue(name, value)
}

// literal renders a value the call strings carry. Anything it does not know
// renders as an empty string literal rather than as Go's %v, which the
// parser would refuse at execute time with the whole write lost.
func literal(v any) string {
	switch x := v.(type) {
	case string:
		return langparser.QuoteString(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "0"
		}
		// 'f', never an exponent: 1e-07 is not a number literal the
		// lexer reads.
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, literal(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, langparser.QuoteString(k)+": "+literal(x[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return `""`
}

func stringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func floatMap(m map[string]float64) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func noteList(notes []Note) []any {
	out := make([]any, 0, len(notes))
	for _, n := range notes {
		out = append(out, map[string]any{"code": n.Code, "message": n.Message})
	}
	return out
}

func stageList(stages []StageSummary) []any {
	out := make([]any, 0, len(stages))
	for _, st := range stages {
		out = append(out, map[string]any{
			"name": st.Name, "status": st.Status, "durationMs": st.DurationMs,
			"steps": int64(st.Steps), "failed": int64(st.Failed),
		})
	}
	return out
}

// formatID renders a GitHub id the concepts store as text.
func formatID(v int64) string { return strconv.FormatInt(v, 10) }

func formatIDIfSet(v int64) string {
	if v <= 0 {
		return ""
	}
	return formatID(v)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// formatTimeIfSet is "" for the zero time rather than year one: a datetime
// nobody set is not a date.
func formatTimeIfSet(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

// constructOf names the construct a rendered call addresses, for an error.
func constructOf(query string) string {
	q := strings.TrimSpace(query)
	if _, rest, ok := strings.Cut(q, " "); ok {
		q = rest
	}
	if i := strings.IndexByte(q, '('); i > 0 {
		return strings.TrimSpace(q[:i])
	}
	return q
}

// normalizeRepository is the one spelling a repository is matched in.
func normalizeRepository(repository string) string {
	return strings.ToLower(strings.TrimSpace(repository))
}

// ---------------------------------------------------------------------------
// Reading rows back
// ---------------------------------------------------------------------------

// rowsOf normalises whatever the engine handed back into flat rows: a shaped
// query's projected rows (every read here is shaped), a bundle's nodes with
// their payload lifted to the top, or a builtin's node map. A copy of the
// shape component/packages' memqlRows reads, kept here rather than shared
// for the reason that one gives: fifteen lines over a wire type that does not
// change, in packages at different tiers.
func rowsOf(res *memql.ExecuteResult) []map[string]any {
	if res == nil {
		return nil
	}
	switch v := res.OutputPayload().(type) {
	case *memqlv1.GraphBundle:
		if v == nil {
			return nil
		}
		out := make([]map[string]any, 0, len(v.GetNodes()))
		for _, n := range v.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{"id": n.GetId(), "concept": n.GetConcept()}
			if payload := n.GetPayload(); payload != nil {
				for k, val := range payload.AsMap() {
					if _, intrinsic := row[k]; !intrinsic {
						row[k] = val
					}
				}
			}
			out = append(out, row)
		}
		return out
	case map[string]memorynodes.MemoryNode:
		return flattenRows(memql.MaterializeRows(v))
	case []map[string]any:
		return flattenRows(v)
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return flattenRows(out)
	case map[string]any:
		if rows, ok := v["rows"].([]any); ok {
			out := make([]map[string]any, 0, len(rows))
			for _, item := range rows {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return flattenRows(out)
		}
		return flattenRows([]map[string]any{v})
	}
	return nil
}

// flattenRows lifts a nested `payload` object to the top of each row, the
// intrinsics (id, createdAt) winning over a payload key of the same name.
func flattenRows(rows []map[string]any) []map[string]any {
	for i, row := range rows {
		payload, ok := row["payload"].(map[string]any)
		if !ok {
			continue
		}
		flat := make(map[string]any, len(row)+len(payload))
		for k, v := range payload {
			flat[k] = v
		}
		for k, v := range row {
			if k != "payload" {
				flat[k] = v
			}
		}
		rows[i] = flat
	}
	return rows
}

func onePipeline(rows []map[string]any, err error) (*Pipeline, error) {
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	p := pipelineFromRow(rows[0])
	return &p, nil
}

func allPipelines(rows []map[string]any, err error) ([]Pipeline, error) {
	if err != nil {
		return nil, err
	}
	out := make([]Pipeline, 0, len(rows))
	for _, row := range rows {
		out = append(out, pipelineFromRow(row))
	}
	return out, nil
}

func oneRun(rows []map[string]any, err error) (*Run, error) {
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := runFromRow(rows[0])
	return &r, nil
}

func allRuns(rows []map[string]any, err error) ([]Run, error) {
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(rows))
	for _, row := range rows {
		out = append(out, runFromRow(row))
	}
	return out, nil
}

func pipelineFromRow(row map[string]any) Pipeline {
	compute := pipelines.Compute(rowString(row, "compute"))
	if compute == "" {
		compute = pipelines.ComputeCluster
	}
	return Pipeline{
		ID:               bareID(rowString(row, "id")),
		OwnerUserID:      rowString(row, "ownerUserId"),
		AccountID:        bareID(rowString(row, "accountId")),
		PackageID:        bareID(rowString(row, "packageId")),
		Name:             rowString(row, "name"),
		Repository:       normalizeRepository(rowString(row, "repository")),
		DefaultBranch:    rowString(row, "defaultBranch"),
		InstallationID:   rowID(row, "installationId"),
		CredentialID:     bareID(rowString(row, "credentialId")),
		Delivery:         rowString(row, "delivery"),
		Compute:          compute,
		Status:           rowString(row, "status"),
		SecretNames:      rowStrings(row, "secretNames"),
		ChannelIDs:       rowStrings(row, "channelIds"),
		Heads:            rowStringMap(row, "heads"),
		Timings:          rowFloatMap(row, "timings"),
		TimingsRunID:     bareID(rowString(row, "timingsRunId")),
		TimingsUpdatedAt: rowTime(row, "timingsUpdatedAt"),
		ConnectedAt:      rowTime(row, "connectedAt"),
	}
}

func runFromRow(row map[string]any) Run {
	r := Run{
		ID:                bareID(rowString(row, "id")),
		OwnerUserID:       rowString(row, "ownerUserId"),
		AccountID:         bareID(rowString(row, "accountId")),
		PipelineID:        bareID(rowString(row, "pipelineId")),
		Repository:        normalizeRepository(rowString(row, "repository")),
		SHA:               strings.ToLower(rowString(row, "sha")),
		Mode:              pipelines.Mode(rowString(row, "mode")),
		Event:             pipelines.Event(rowString(row, "event")),
		RunKey:            rowString(row, "runKey"),
		Attempt:           rowInt(row, "attempt"),
		Trigger:           rowString(row, "trigger"),
		RerunOf:           bareID(rowString(row, "rerunOf")),
		DeliveryID:        rowString(row, "deliveryId"),
		PullRequest:       rowInt(row, "pullRequest"),
		HeadBranch:        rowString(row, "headBranch"),
		BaseSHA:           strings.ToLower(rowString(row, "baseSha")),
		Title:             rowString(row, "title"),
		Version:           rowString(row, "version"),
		Status:            rowString(row, "status"),
		Conclusion:        rowString(row, "conclusion"),
		RefusalCode:       rowString(row, "refusalCode"),
		RefusalMessage:    rowString(row, "refusalMessage"),
		RefusalScope:      rowString(row, "refusalScope"),
		CheckRunID:        rowID(row, "checkRunId"),
		CheckRunState:     rowString(row, "checkRunState"),
		WorkRunID:         rowString(row, "workRunId"),
		WorkGoalID:        bareID(rowString(row, "workGoalId")),
		DriverNodeID:      rowString(row, "driverNodeId"),
		DriverHeartbeatAt: rowTime(row, "driverHeartbeatAt"),
		CancelRequested:   rowBool(row, "cancelRequested"),
		CancelledBy:       rowString(row, "cancelledBy"),
		QueuedAt:          rowTime(row, "queuedAt"),
		StartedAt:         rowTime(row, "startedAt"),
		FinishedAt:        rowTime(row, "finishedAt"),
		DurationMs:        rowInt64(row, "durationMs"),
		CreatedAt:         rowTime(row, "createdAt"),
	}
	for _, obj := range rowObjects(row, "notes") {
		r.Notes = append(r.Notes, Note{Code: rowString(obj, "code"), Message: rowString(obj, "message")})
	}
	for _, obj := range rowObjects(row, "stages") {
		r.Stages = append(r.Stages, StageSummary{
			Name:       rowString(obj, "name"),
			Status:     rowString(obj, "status"),
			DurationMs: rowInt64(obj, "durationMs"),
			Steps:      rowInt(obj, "steps"),
			Failed:     rowInt(obj, "failed"),
		})
	}
	return r
}

func packageFromRow(row map[string]any) PackageSource {
	return PackageSource{
		ID:           bareID(rowString(row, "id")),
		OwnerUserID:  rowString(row, "ownerUserId"),
		AccountID:    bareID(rowString(row, "accountId")),
		Name:         rowString(row, "name"),
		SourceKind:   rowString(row, "sourceKind"),
		RepoURL:      rowString(row, "repoUrl"),
		CredentialID: bareID(rowString(row, "credentialId")),
		Status:       rowString(row, "status"),
	}
}

func rowString(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	switch v := row[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	}
	return ""
}

// rowInt64 reads a payload number. A float64 arm saturates (ClampFloat64ToInt64)
// rather than converting bare: an out-of-range conversion is
// implementation-defined, and a count read as a huge negative inverts every
// guard on it.
func rowInt64(row map[string]any, key string) int64 {
	if row == nil {
		return 0
	}
	switch v := row[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return num.ClampFloat64ToInt64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// rowInt narrows rowInt64 to an int, saturating: every caller reads a
// count or an ordinal, where saturation is the answer that keeps order.
func rowInt(row map[string]any, key string) int { return num.ClampInt64(rowInt64(row, key)) }

// rowID reads a GitHub id the concepts store as text (installationId,
// checkRunId). Anything that is not a positive number is 0: no id.
func rowID(row map[string]any, key string) int64 {
	n := rowInt64(row, key)
	if n < 0 {
		return 0
	}
	return n
}

func rowBool(row map[string]any, key string) bool {
	if row == nil {
		return false
	}
	switch v := row[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

func rowStrings(row map[string]any, key string) []string {
	if row == nil {
		return nil
	}
	var out []string
	switch v := row[key].(type) {
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

func rowStringMap(row map[string]any, key string) map[string]string {
	if row == nil {
		return nil
	}
	out := map[string]string{}
	switch v := row[key].(type) {
	case map[string]string:
		for k, s := range v {
			out[k] = s
		}
	case map[string]any:
		for k, item := range v {
			if s, ok := item.(string); ok {
				out[k] = s
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func rowFloatMap(row map[string]any, key string) map[string]float64 {
	if row == nil {
		return nil
	}
	out := map[string]float64{}
	switch v := row[key].(type) {
	case map[string]float64:
		for k, f := range v {
			out[k] = f
		}
	case map[string]any:
		for k, item := range v {
			switch f := item.(type) {
			case float64:
				out[k] = f
			case int64:
				out[k] = float64(f)
			case int:
				out[k] = float64(f)
			case json.Number:
				if parsed, err := f.Float64(); err == nil {
					out[k] = parsed
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func rowObjects(row map[string]any, key string) []map[string]any {
	if row == nil {
		return nil
	}
	var out []map[string]any
	switch v := row[key].(type) {
	case []map[string]any:
		out = append(out, v...)
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// rowTime reads a datetime, the zero time for an absent or unreadable one.
func rowTime(row map[string]any, key string) time.Time {
	if row == nil {
		return time.Time{}
	}
	switch v := row[key].(type) {
	case time.Time:
		return v.UTC()
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}
