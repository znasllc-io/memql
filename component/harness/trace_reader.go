package harness

// trace_reader.go is the bun-backed feeder for the pure trace assembler
// (trace.go). It reads EVERY version of a plan and its steps + all the
// observations recorded against it, straight from the MemoryNodes
// hypertable, and projects each row version into a TraceEvent. The pure
// AssembleTrace then orders + rolls them up.
//
// This reads the append-only event stream directly via bun -- mirroring
// the reconciler's BunStepReader (adapters.go) and the read pattern in
// cognition_participant_validation.go. Unlike the reconciler reader (which
// dedupes to latest-per-id), the trace reader keeps EVERY version: a plan's
// open->running->done transitions are three rows, and each one is a
// timeline event named by its provenanceMutation. That is the whole point
// of the trace -- the transitions are the story.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TraceReader assembles a plan's full timeline from the graph. It is the
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

// Trace reconstructs the full ordered timeline for planID: every plan +
// step version transition and every observation, ordered by createdAt.
// Returns an empty (non-error) trace when the plan does not exist.
func (r *TraceReader) Trace(ctx context.Context, planID string) (Trace, error) {
	db := r.handle()
	if db == nil {
		return Trace{}, fmt.Errorf("harness trace reader: database not configured")
	}

	events := make([]TraceEvent, 0, 64)

	planEvents, err := r.planEvents(ctx, db, planID)
	if err != nil {
		return Trace{}, err
	}
	events = append(events, planEvents...)

	stepEvents, err := r.stepEvents(ctx, db, planID)
	if err != nil {
		return Trace{}, err
	}
	events = append(events, stepEvents...)

	obsEvents, err := r.observationEvents(ctx, db, planID)
	if err != nil {
		return Trace{}, err
	}
	events = append(events, obsEvents...)

	return AssembleTrace(planID, events), nil
}

// planEvents reads every version of the plan row (one per transition).
//
// staged-data: MUST-NOT-GATE -- and the same ruling covers stepEvents and
// observationEvents below (epic memql#3974, task memql#3984).
//
// These three assemble an AUDIT TRACE, and a trace with holes does not read as
// "some rows were withheld" -- it reads as "this never happened". That is a
// worse outcome than disclosure: an operator reconstructing what a plan did
// would conclude a transition never occurred, and the absence carries no marker
// saying otherwise.
//
// Note also that these reads deliberately take EVERY version rather than the
// latest (the ASC ordering and the per-version TraceEvent below are the point),
// so they are not a read of current state at all. The tier withholds current
// rows from the ordinary read path; the history of transitions is a different
// question, asked by a different consumer, for a purpose the tier does not
// speak to.
func (r *TraceReader) planEvents(ctx context.Context, db *bun.DB, planID string) ([]TraceEvent, error) {
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptHarnessPlan).
		Where("id = ?", planID).
		OrderExpr(`"createdAt" ASC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan plan versions for %q: %w", planID, err)
	}
	out := make([]TraceEvent, 0, len(nodes))
	for _, n := range nodes {
		payload := unmarshalPayload(n.Payload)
		out = append(out, TraceEvent{
			At:       n.CreatedAt,
			Kind:     EventKindPlan,
			NodeID:   n.ID,
			Status:   stringField(payload, "status"),
			Mutation: stringField(payload, "provenanceMutation"),
			Actor:    n.CreatedBy,
			Content:  stringField(payload, "goal"),
		})
	}
	return out, nil
}

// stepEvents reads every version of every step belonging to the plan.
//
// staged-data: MUST-NOT-GATE -- see planEvents; an audit trace with holes reads
// as "this never happened".
func (r *TraceReader) stepEvents(ctx context.Context, db *bun.DB, planID string) ([]TraceEvent, error) {
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptHarnessStep).
		Where("payload->>'planId' = ?", planID).
		OrderExpr(`"createdAt" ASC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan step versions for plan %q: %w", planID, err)
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
			Content:  stringField(payload, "title"),
		})
	}
	return out, nil
}

// observationEvents reads all observations for the plan. Observations are
// append-only (one row per event), so there are no versions to fold.
//
// staged-data: MUST-NOT-GATE -- see planEvents; an audit trace with holes reads
// as "this never happened".
func (r *TraceReader) observationEvents(ctx context.Context, db *bun.DB, planID string) ([]TraceEvent, error) {
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptHarnessObservation).
		Where("payload->>'planId' = ?", planID).
		OrderExpr(`"createdAt" ASC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan observations for plan %q: %w", planID, err)
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
