// Package worktrace exposes the `workTrace` DSL/SDK builtin -- the
// history-over-gRPC contract a remote client (the memql-cockpit CLI)
// calls to fetch a run's full execution timeline (issue
// memql-cockpit#142; re-keyed onto the work journal by the work spine's
// epic A1).
//
// Why this exists: the trace assembler (#589) reconstructs a run's
// timeline from the append-only graph event stream, but it was only
// reachable in-process. This integration is the remote half: it wraps
// NewTraceReader behind an @sdk builtin so the cockpit can call it
// through the typed Go SDK like any other engine query -- no bespoke wire
// wrapper, no raw DSL string.
//
// The assembler it wraps used to live in component/harness and moved here
// with the harness retirement, re-pointed from v1:harness:plan/step/
// observation to the work journal's v1:work:run/step/observation. The
// SHAPE of what it returns is unchanged, so the cockpit's decoder still
// reads it -- only the arg name moved, planId to runId.
//
// DSL surface (dsl/memory/builtins.memql -> builtin workTrace):
//
//	workTrace(runId: "v1:work:run:...")
//
// Returns a SINGLE synthetic node whose JSON payload carries:
//
//	{
//	  "runId":     "<the requested run id>",
//	  "timeline":  "<tr.Render() -- the human-readable timeline>",
//	  "complete":  <bool -- tr.IsComplete(), the run reached a terminal status>,
//	  "stepCount": <int -- len(tr.Steps)>
//	}
//
// OWNER-SCOPING IS UPSTREAM, AND THE READER IS DELIBERATELY NOT SCOPED.
// v1:work:run and v1:work:step declare the composite owner tier, but the
// reader goes to raw SQL by run id -- because a TRACE with holes does not
// read as "rows were withheld", it reads as "this never happened", which
// is a worse failure than disclosure for an audit surface. The same
// reasoning is recorded per-query in reader.go under the staged-data
// ruling. We resolve the calling actor here and log it for the audit
// trail; narrowing the read itself is an owner decision that belongs with
// the surface that exposes traces to more than their own owner.
package worktrace

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	coreid "github.com/znasllc-io/memql/core/id"
)

// runConcept is the concept id every v1:work:run row is stored
// under. The trace reader matches the CANONICAL stored id (`id = ?` on
// the run row and `payload->>'runId' = ?` on step/observation rows),
// so a bare runId arg is composed to canonical form before the lookup.
const runConcept = memorynodes.ConceptWorkRun

// traceConcept is the synthetic concept stamped on the single returned
// node. It is not a real graph concept (this builtin reads, never
// writes) -- it just labels the envelope so a consumer inspecting the
// result knows what it is looking at.
const traceConcept = "v1:work:trace"

// Integration holds the state the workTrace handler needs:
// a lazily-resolved bun handle (the trace reader reads via bun). It is
// constructed by the plug-in factory and populated via SetDBGetter at
// registration time, mirroring integrations/harnessrecall.
type Integration struct {
	Logger *slog.Logger

	dbGetter func() *bun.DB
}

// New constructs a workTrace integration.
func New(logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{Logger: logger}
}

// SetDBGetter injects the lazy bun handle getter. The trace assembler
// (NewTraceReader) takes exactly this shape, so we hand it
// straight through -- no adapter.
func (i *Integration) SetDBGetter(f func() *bun.DB) { i.dbGetter = f }

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "workTrace" }

// Capabilities lists the DSL-callable functions. One capability:
// trace.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "trace",
			Description: "Fetch a run's full execution timeline (every run/step version transition plus all observations, ordered by createdAt) reconstructed from the append-only graph event stream. Returns one synthetic node carrying the rendered timeline, completion flag, and step count. The history-over-gRPC contract for the cockpit's trace CLI.",
			Handler:     i.traceHandler,
			ArgsSchema: map[string]string{
				"runId": "string (required) - the v1:work:run id whose timeline to reconstruct",
			},
		},
	}
}

// traceHandler is the single DSL-facing entry point. It resolves the
// runId arg, runs the existing harness trace assembler, and packs the
// result into one synthetic MemoryNode.
func (i *Integration) traceHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	runID, err := parseRunID(args)
	if err != nil {
		return nil, err
	}

	if i.dbGetter == nil || i.dbGetter() == nil {
		return nil, fmt.Errorf("workTrace.trace: database not configured")
	}

	// Owner-scope: the trace is for the caller's own plan. Resolve the
	// authenticated actor (mirroring recall) for the audit trail. Empty
	// in dev mode / no auth.
	owner := ownerFromContext(ctx)

	reader := NewTraceReader(i.dbGetter)
	tr, err := reader.Trace(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("workTrace.trace: assemble trace for %q: %w", runID, err)
	}

	node, err := buildTraceNode(runID, tr)
	if err != nil {
		return nil, fmt.Errorf("workTrace.trace: build node for %q: %w", runID, err)
	}

	i.Logger.Info("workTrace.trace: completed",
		"runId", runID,
		"owner", owner,
		"complete", tr.IsComplete(),
		"step_count", len(tr.Steps),
		"event_count", len(tr.Events),
	)
	return []memorynodes.MemoryNode{node}, nil
}

// parseRunID validates the required runId arg and canonicalizes it to
// the run concept. Post-#2441 the tool loop hands agents BARE
// shortIds, so a bare arg is composed to `v1:work:run:<short>`
// before the trace reader's canonical-id lookup; an internal caller
// passing the already-canonical id is a no-op; an id under a DIFFERENT
// concept is rejected loudly instead of silently matching zero rows.
func parseRunID(args map[string]any) (string, error) {
	runID, _ := args["runId"].(string)
	return canonicalizeRunID(runID)
}

// canonicalizeRunID normalizes a bare-or-canonical run id to the
// canonical `v1:work:run:<short>` form the trace reader matches.
// Mirrors the engine's canonicalizeIdValue behavior for a fixed target
// concept (no registry needed -- the target is known): bare composes,
// canonical passes through, wrong-concept errors, empty errors.
func canonicalizeRunID(runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return "", fmt.Errorf("workTrace.trace: runId is required")
	}
	// Bare shortId (no concept prefix): compose the canonical run id.
	// Stored shortIds are colon-free (A0), so a colon means the value is
	// already concept-qualified.
	if !strings.ContainsRune(runID, ':') {
		return coreid.BuildNodeId(runConcept, runID), nil
	}
	// Already the run concept -> pass through.
	if strings.HasPrefix(runID, runConcept+":") {
		return runID, nil
	}
	// Concept-qualified under a different concept -> caller passed the
	// wrong id type.
	gotConcept, _, err := coreid.ParseNodeId(runID)
	if err != nil {
		return "", fmt.Errorf("workTrace.trace: malformed runId %q: %w", runID, err)
	}
	if gotConcept != "" && gotConcept != runConcept {
		return "", fmt.Errorf("workTrace.trace: runId %q is under concept %q, expected %q", runID, gotConcept, runConcept)
	}
	return runID, nil
}

// ownerFromContext resolves the authenticated actor's userId from the
// auth context (mirrors recall's owner handling). Empty when no auth
// context is present (dev mode).
func ownerFromContext(ctx context.Context) string {
	if ac, ok := componentAuth.AccessFromContext(ctx); ok && ac != nil {
		return strings.TrimSpace(ac.UserId)
	}
	return ""
}

// tracePayload is the JSON shape carried back on the synthetic node's
// Payload. It is the wire contract the cockpit's trace CLI
// decodes.
type tracePayload struct {
	RunID     string `json:"runId"`
	Timeline  string `json:"timeline"`
	Complete  bool   `json:"complete"`
	StepCount int    `json:"stepCount"`
}

// buildTraceNode packs an assembled Trace into the single synthetic
// MemoryNode the handler returns. The id is deterministic so repeated
// calls for the same plan return a stable node id (the trace is a
// read-through view, not a new graph row).
func buildTraceNode(runID string, tr Trace) (memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(tracePayload{
		RunID:     runID,
		Timeline:  tr.Render(),
		Complete:  tr.IsComplete(),
		StepCount: len(tr.Steps),
	})
	if err != nil {
		return memorynodes.MemoryNode{}, fmt.Errorf("marshal payload: %w", err)
	}
	return memorynodes.MemoryNode{
		ID:        "work-trace:" + runID,
		Concept:   traceConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}, nil
}
