package pipelinerun

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
// rename in dsl/pipelines is one edit here, and a test reads the .memql files
// and holds every rendered call -- construct and argument names -- to them.
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
func (s *dslStore) callerRead(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, errNoStore
	}
	query, err := render("query", name, args)
	if err != nil {
		return nil, err
	}
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pipelines: %s: %w", name, err)
	}
	return rowsOf(res), nil
}

// systemRead runs a server-only read as this package's own system actor. It
// REPLACES whatever actor ctx carried: a delivery, a schedule or a driver has
// no person behind it, and a person-facing caller that reaches one of these
// has already been judged by an owner-scoped read of its own.
func (s *dslStore) systemRead(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	query, err := render("query", name, args)
	if err != nil {
		return nil, err
	}
	return s.executeInternal(auth.ContextWithSystemActor(ctx, systemActorName), name, query)
}

// ownerWrite runs a write under owner's borrowed authority. The mutations
// stamp ownerUserId from the actor, so the actor must BE the owner -- and an
// empty owner is refused here, before anything is written: auth's helper
// leaves ctx untouched for a blank id, which would write the row under
// whichever actor the caller happened to carry.
func (s *dslStore) ownerWrite(ctx context.Context, owner, name string, args map[string]any) error {
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("pipelines: %s: the row's owner is unknown, and a pipelines row is written only under its owner's authority", name)
	}
	query, err := render("mutation", name, args)
	if err != nil {
		return err
	}
	_, err = s.executeInternal(auth.ContextWithUserActor(ctx, strings.TrimSpace(owner)), name, query)
	return err
}

// executeInternal is THE ONE PLACE this package stamps internal origin, and
// it stamps INLINE, as the argument to the one Execute that needs it, so the
// mark dies at that call and no later frame inherits it (memql#2879,
// memql#2989; internal_origin_test.go counts this site). Every server-only
// read and every write funnels through here; the actor was chosen by the
// caller above, which is the whole of what the stamp does NOT decide.
func (s *dslStore) executeInternal(actorCtx context.Context, name, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, errNoStore
	}
	res, err := s.engine.Execute(auth.ContextWithInternalOrigin(actorCtx), query)
	if err != nil {
		return nil, fmt.Errorf("pipelines: %s: %w", name, err)
	}
	return rowsOf(res), nil
}

// render is "<kind> <name>(<args>)" through langparser.RenderCall, the one
// renderer for a MemQL call composed in Go: named arguments in sorted order,
// every value JSON-encoded so none can break out of its literal (QuoteString's
// escape set, never %q's). An argument absent from args is not rendered,
// which is how an optional one is omitted.
func render(kind, name string, args map[string]any) (string, error) {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return "", fmt.Errorf("pipelines: rendering %s: %w", name, err)
	}
	return kind + " " + call, nil
}

// ---------------------------------------------------------------------------
// Person-facing reads
// ---------------------------------------------------------------------------

func (s *dslStore) PackageForCaller(ctx context.Context, packageID string) (*PackageSource, error) {
	rows, err := s.callerRead(ctx, qPackageByID, map[string]any{"packageId": bareID(packageID)})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	p := packageFromRow(rows[0])
	return &p, nil
}

func (s *dslStore) PipelineForOwner(ctx context.Context, pipelineID string) (*Pipeline, error) {
	return onePipeline(s.callerRead(ctx, qPipelineForOwner, map[string]any{"pipelineId": bareID(pipelineID)}))
}

func (s *dslStore) PipelineForPackage(ctx context.Context, packageID string) (*Pipeline, error) {
	return onePipeline(s.callerRead(ctx, qPipelineForPackage, map[string]any{"packageId": bareID(packageID)}))
}

func (s *dslStore) PipelinesForOwner(ctx context.Context) ([]Pipeline, error) {
	return allPipelines(s.callerRead(ctx, qPipelinesForOwner, nil))
}

func (s *dslStore) RunForOwner(ctx context.Context, runID string) (*Run, error) {
	return oneRun(s.callerRead(ctx, qPipelineRunForOwner, map[string]any{"runId": bareID(runID)}))
}

func (s *dslStore) RunsForOwner(ctx context.Context, pipelineID string) ([]Run, error) {
	// The pipeline is optional: absent lists every pipeline's runs.
	args := map[string]any{}
	setIfSet(args, "pipelineId", bareID(pipelineID))
	return allRuns(s.callerRead(ctx, qPipelineRunsForOwner, args))
}

// ---------------------------------------------------------------------------
// Server-only reads
// ---------------------------------------------------------------------------

func (s *dslStore) PipelinesForRepository(ctx context.Context, repository string) ([]Pipeline, error) {
	return allPipelines(s.systemRead(ctx, qPipelinesForRepository, map[string]any{"repository": normalizeRepository(repository)}))
}

func (s *dslStore) PipelinesPolled(ctx context.Context) ([]Pipeline, error) {
	return allPipelines(s.systemRead(ctx, qPipelinesPolled, nil))
}

func (s *dslStore) PipelineByID(ctx context.Context, pipelineID string) (*Pipeline, error) {
	return onePipeline(s.systemRead(ctx, qPipelineByID, map[string]any{"pipelineId": bareID(pipelineID)}))
}

func (s *dslStore) RunsForKey(ctx context.Context, runKey string) ([]Run, error) {
	return allRuns(s.systemRead(ctx, qPipelineRunsForKey, map[string]any{"runKey": runKey}))
}

func (s *dslStore) RunsForPipelineSHA(ctx context.Context, pipelineID, sha string) ([]Run, error) {
	return allRuns(s.systemRead(ctx, qPipelineRunsForPipelineSha, map[string]any{
		"pipelineId": bareID(pipelineID),
		"sha":        strings.ToLower(strings.TrimSpace(sha)),
	}))
}

func (s *dslStore) RunByCheckRun(ctx context.Context, repository string, checkRunID int64) (*Run, error) {
	if checkRunID <= 0 {
		return nil, nil
	}
	return oneRun(s.systemRead(ctx, qPipelineRunByCheckRun, map[string]any{
		"repository": normalizeRepository(repository),
		"checkRunId": formatID(checkRunID),
	}))
}

func (s *dslStore) RunsUnfinished(ctx context.Context) ([]Run, error) {
	return allRuns(s.systemRead(ctx, qPipelineRunsUnfinished, nil))
}

func (s *dslStore) RunByID(ctx context.Context, runID string) (*Run, error) {
	return oneRun(s.systemRead(ctx, qPipelineRunByID, map[string]any{"runId": bareID(runID)}))
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
	args := map[string]any{
		"pipelineId":     bareID(p.ID),
		"packageId":      bareID(p.PackageID),
		"name":           p.Name,
		"repository":     normalizeRepository(p.Repository),
		"installationId": formatID(p.InstallationID),
		"credentialId":   bareID(p.CredentialID),
		"delivery":       p.Delivery,
		"compute":        string(compute),
		"secretNames":    stringList(p.SecretNames),
	}
	setIfSet(args, "accountId", bareID(p.AccountID))
	setIfSet(args, "defaultBranch", p.DefaultBranch)
	if len(p.ChannelIDs) > 0 {
		args["channelIds"] = stringList(p.ChannelIDs)
	}
	return s.ownerWrite(ctx, p.OwnerUserID, mCreatePipeline, args)
}

// UpdatePipeline writes the named fields of patch and nothing else.
func (s *dslStore) UpdatePipeline(ctx context.Context, owner, pipelineID string, patch PipelinePatch) error {
	args := map[string]any{"pipelineId": bareID(pipelineID)}
	setNamed(args, "name", patch.Name)
	setNamed(args, "defaultBranch", patch.DefaultBranch)
	setNamed(args, "delivery", patch.Delivery)
	setNamed(args, "status", patch.Status)
	if patch.Compute != nil {
		args["compute"] = string(*patch.Compute)
	}
	if patch.SecretNames != nil {
		args["secretNames"] = stringList(*patch.SecretNames)
	}
	if patch.ChannelIDs != nil {
		args["channelIds"] = stringList(*patch.ChannelIDs)
	}
	if patch.Heads != nil {
		args["heads"] = stringMap(*patch.Heads)
	}
	if patch.Timings != nil {
		args["timings"] = floatMap(*patch.Timings)
	}
	if patch.TimingsRunID != nil {
		args["timingsRunId"] = bareID(*patch.TimingsRunID)
	}
	if patch.TimingsUpdatedAt != nil {
		args["timingsUpdatedAt"] = formatTimeIfSet(*patch.TimingsUpdatedAt)
	}
	return s.ownerWrite(ctx, owner, mUpdatePipeline, args)
}

// CreateRun opens a run with every open-time field. An empty optional field
// is OMITTED rather than written empty: the row is new, so absent is the
// truth, and the mutation stamps conclusion and checkRunState empty itself.
func (s *dslStore) CreateRun(ctx context.Context, r Run) error {
	queued := r.QueuedAt
	if queued.IsZero() {
		queued = time.Now().UTC()
	}
	args := map[string]any{
		"runId":      bareID(r.ID),
		"pipelineId": bareID(r.PipelineID),
		"repository": normalizeRepository(r.Repository),
		"sha":        strings.ToLower(strings.TrimSpace(r.SHA)),
		"mode":       string(r.Mode),
		"event":      string(r.Event),
		"runKey":     r.RunKey,
		"attempt":    int64(r.Attempt),
		"trigger":    r.Trigger,
		"status":     r.Status,
		"queuedAt":   formatTime(queued),
	}
	setIfSet(args, "accountId", bareID(r.AccountID))
	setIfSet(args, "rerunOf", bareID(r.RerunOf))
	setIfSet(args, "deliveryId", r.DeliveryID)
	setIfSet(args, "headBranch", r.HeadBranch)
	setIfSet(args, "baseSha", strings.ToLower(strings.TrimSpace(r.BaseSHA)))
	setIfSet(args, "title", r.Title)
	setIfSet(args, "version", r.Version)
	setIfSet(args, "conclusion", r.Conclusion)
	setIfSet(args, "refusalCode", r.RefusalCode)
	setIfSet(args, "refusalMessage", r.RefusalMessage)
	setIfSet(args, "refusalScope", r.RefusalScope)
	setIfSet(args, "checkRunId", formatIDIfSet(r.CheckRunID))
	setIfSet(args, "checkRunState", r.CheckRunState)
	setIfSet(args, "finishedAt", formatTimeIfSet(r.FinishedAt))
	if r.PullRequest > 0 {
		args["pullRequest"] = int64(r.PullRequest)
	}
	if len(r.Notes) > 0 {
		args["notes"] = noteList(r.Notes)
	}
	if r.DurationMs > 0 {
		args["durationMs"] = r.DurationMs
	}
	return s.ownerWrite(ctx, r.OwnerUserID, mCreatePipelineRun, args)
}

// UpdateRun writes the named fields of patch and nothing else.
func (s *dslStore) UpdateRun(ctx context.Context, owner, runID string, patch RunPatch) error {
	args := map[string]any{"runId": bareID(runID)}
	setNamed(args, "status", patch.Status)
	setNamed(args, "conclusion", patch.Conclusion)
	setNamed(args, "refusalCode", patch.RefusalCode)
	setNamed(args, "refusalMessage", patch.RefusalMessage)
	setNamed(args, "refusalScope", patch.RefusalScope)
	setNamed(args, "checkRunState", patch.CheckRunState)
	setNamed(args, "workRunId", patch.WorkRunID)
	setNamed(args, "driverNodeId", patch.DriverNodeID)
	setNamed(args, "cancelledBy", patch.CancelledBy)
	if patch.CheckRunID != nil {
		args["checkRunId"] = formatIDIfSet(*patch.CheckRunID)
	}
	if patch.Notes != nil {
		args["notes"] = noteList(*patch.Notes)
	}
	if patch.WorkGoalID != nil {
		args["workGoalId"] = bareID(*patch.WorkGoalID)
	}
	if patch.DriverHeartbeatAt != nil {
		args["driverHeartbeatAt"] = formatTimeIfSet(*patch.DriverHeartbeatAt)
	}
	if patch.CancelRequested != nil {
		args["cancelRequested"] = *patch.CancelRequested
	}
	if patch.Stages != nil {
		args["stages"] = stageList(*patch.Stages)
	}
	if patch.StartedAt != nil {
		args["startedAt"] = formatTimeIfSet(*patch.StartedAt)
	}
	if patch.FinishedAt != nil {
		args["finishedAt"] = formatTimeIfSet(*patch.FinishedAt)
	}
	if patch.DurationMs != nil {
		args["durationMs"] = *patch.DurationMs
	}
	return s.ownerWrite(ctx, owner, mUpdatePipelineRun, args)
}

// ---------------------------------------------------------------------------
// Argument values
// ---------------------------------------------------------------------------
//
// Every collection is built NON-NIL here: RenderCall JSON-encodes, and a nil
// slice or map encodes as `null`, a spelling the parser refuses (edition 2026)
// and a type no concept field accepts.

// setIfSet puts a string argument only when it is not blank: an optional
// field nobody set is omitted, never written empty.
func setIfSet(args map[string]any, name, value string) {
	if strings.TrimSpace(value) != "" {
		args[name] = value
	}
}

// setNamed puts a patch field when the patch names it, its empty value
// included -- that is how a read-merge write clears a field.
func setNamed(args map[string]any, name string, value *string) {
	if value != nil {
		args[name] = *value
	}
}

func stringList(values []string) []string {
	out := make([]string, 0, len(values))
	return append(out, values...)
}

func stringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// floatMap is a timing table as an argument. A NaN or an infinity has no JSON
// spelling and would refuse the whole write, so such an entry is dropped: an
// unmeasured package weighs pipelines.UnknownSeconds at the next split, which
// is the truth about it.
func floatMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out[k] = v
		}
	}
	return out
}

func noteList(notes []Note) []Note {
	out := make([]Note, 0, len(notes))
	return append(out, notes...)
}

func stageList(stages []StageSummary) []StageSummary {
	out := make([]StageSummary, 0, len(stages))
	return append(out, stages...)
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

// constructOf names the construct a rendered call addresses, for a test or a
// log line.
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
