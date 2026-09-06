package worktrace

// reader.go is the bun-backed feeder for the pure trace assembler
// (timeline.go). It reads EVERY version of a run and its steps + all the
// observations recorded against it, straight from the MemoryNodes
// hypertable, and projects each row version into a TraceEvent. The pure
// AssembleTrace then orders + rolls them up.
//
// This reads the append-only event stream directly via bun. Unlike a
// current-state read (which dedupes to latest-per-id), the trace reader
// keeps EVERY version: a run's running -> succeeded transitions are two
// rows and a retried step is three, and each one is a timeline event. That
// is the whole point of the trace -- the transitions are the story.
//
// RAW SQL, NOT THE ENGINE, AND DELIBERATELY. v1:work:run and v1:work:step
// declare the composite owner tier, and a system run's rows carry a
// present-and-empty owner. Reading them through the DSL would answer the
// CURRENT-STATE question the tier is about; this asks a different one, and
// the staged-data ruling below is the same reasoning one layer down.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TraceReader assembles a run's full timeline from the graph. It is the
// reusable, importable surface the cockpit CLI (and eval) call.
type TraceReader struct {
	db func() *bun.DB
}

// NewTraceReader builds a trace reader over a lazily-resolved bun handle
// (same shape as NewBunStepReader so app wiring is symmetric).
func NewTraceReader(db func() *bun.DB) *TraceReader {
	return &TraceReader{db: db}
}

func (r *TraceReader) handle() *bun.DB {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db()
}

// Trace reconstructs the full ordered timeline for runID: every run +
// step version transition and every observation, ordered by createdAt.
// Returns an empty (non-error) trace when the run does not exist.
func (r *TraceReader) Trace(ctx context.Context, runID string) (Trace, error) {
	db := r.handle()
	if db == nil {
		return Trace{}, fmt.Errorf("work trace reader: database not configured")
	}

	events := make([]TraceEvent, 0, 64)

	runEvents, err := r.runEvents(ctx, db, runID)
	if err != nil {
		return Trace{}, err
	}
	events = append(events, runEvents...)

	stepEvents, err := r.stepEvents(ctx, db, runID)
	if err != nil {
		return Trace{}, err
	}
	events = append(events, stepEvents...)

	obsEvents, err := r.observationEvents(ctx, db, runID)
	if err != nil {
		return Trace{}, err
	}
	events = append(events, obsEvents...)

	return AssembleTrace(runID, events), nil
}

// runEvents reads every version of the run row (one per boundary).
//
// staged-data: MUST-NOT-GATE -- and the same ruling covers stepEvents and
// observationEvents below (epic memql#3974, task memql#3984).
//
// These three assemble an AUDIT TRACE, and a trace with holes does not read as
// "some rows were withheld" -- it reads as "this never happened". That is a
// worse outcome than disclosure: an operator reconstructing what a run did
// would conclude a transition never occurred, and the absence carries no marker
// saying otherwise.
//
// Note also that these reads deliberately take EVERY version rather than the
// latest (the ASC ordering and the per-version TraceEvent below are the point),
// so they are not a read of current state at all. The tier withholds current
// rows from the ordinary read path; the history of transitions is a different
// question, asked by a different consumer, for a purpose the tier does not
// speak to.
func (r *TraceReader) runEvents(ctx context.Context, db *bun.DB, runID string) ([]TraceEvent, error) {
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptWorkRun).
		Where("id = ?", runID).
		OrderExpr(`"createdAt" ASC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan run versions for %q: %w", runID, err)
	}
	out := make([]TraceEvent, 0, len(nodes))
	for _, n := range nodes {
		payload := unmarshalPayload(n.Payload)
		out = append(out, TraceEvent{
			At:       n.CreatedAt,
			Kind:     EventKindRun,
			NodeID:   n.ID,
			Status:   stringField(payload, "status"),
			Mutation: stringField(payload, "provenanceMutation"),
			Actor:    n.CreatedBy,
			Content:  stringField(payload, "automationName"),
		})
	}
	return out, nil
}

// stepEvents reads every version of every step belonging to the run.
//
// staged-data: MUST-NOT-GATE -- see runEvents; an audit trace with holes reads
// as "this never happened".
func (r *TraceReader) stepEvents(ctx context.Context, db *bun.DB, runID string) ([]TraceEvent, error) {
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptWorkStep).
		Where("payload->>'runId' = ?", runID).
		OrderExpr(`"createdAt" ASC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan step versions for run %q: %w", runID, err)
	}
	out := make([]TraceEvent, 0, len(nodes))
	for _, n := range nodes {
		payload := unmarshalPayload(n.Payload)
		out = append(out, TraceEvent{
			At:       n.CreatedAt,
			Kind:     EventKindStep,
			NodeID:   n.ID,
			StepID:   n.ID,
			Status:   stringField(payload, "status"),
			Mutation: stringField(payload, "provenanceMutation"),
			Actor:    n.CreatedBy,
			Content:  stringField(payload, "key"),
		})
	}
	return out, nil
}

// observationEvents reads all observations for the run. Observations are
// append-only (one row per event), so there are no versions to fold.
//
// staged-data: MUST-NOT-GATE -- see runEvents; an audit trace with holes reads
// as "this never happened".
func (r *TraceReader) observationEvents(ctx context.Context, db *bun.DB, runID string) ([]TraceEvent, error) {
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptWorkObservation).
		Where("payload->>'runId' = ?", runID).
		OrderExpr(`"createdAt" ASC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan observations for run %q: %w", runID, err)
	}
	out := make([]TraceEvent, 0, len(nodes))
	for _, n := range nodes {
		payload := unmarshalPayload(n.Payload)
		// The observation's structured payload lives under `data` (not
		// `payload`) per the #582 concept gotcha.
		data := objectField(payload, "data")
		out = append(out, TraceEvent{
			At:              n.CreatedAt,
			Kind:            EventKindObservation,
			NodeID:          n.ID,
			StepID:          stringField(payload, "stepId"),
			ObservationKind: stringField(payload, "kind"),
			Actor:           n.CreatedBy,
			Content:         stringField(payload, "content"),
			ToolName:        stringField(data, "toolName"),
			Data:            data,
		})
	}
	return out, nil
}

// unmarshalPayload decodes a node's JSONB payload, returning an empty map on
// any decode error so a single malformed row never breaks the whole trace.
func unmarshalPayload(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}
