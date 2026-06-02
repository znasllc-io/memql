// Package harnesstrace exposes the `harnessTrace` DSL/SDK builtin -- the
// history-over-gRPC contract a remote client (the memql-cockpit CLI)
// calls to fetch a plan's full execution timeline (issue
// memql-cockpit#142, phase 1; epic #590).
//
// Why this exists: the trace assembler shipped in component/harness
// (#589) reconstructs a plan's timeline from the append-only graph
// event stream, but it was only reachable in-process (eval, replay).
// The #589 agent flagged the missing piece as "no way for a remote
// client to fetch a trace over the wire." This integration is that
// piece: it wraps harness.NewTraceReader behind an @sdk builtin so the
// cockpit can call it through the typed Go SDK like any other engine
// query -- no bespoke wire wrapper, no raw DSL string.
//
// It REUSES the existing assembler verbatim. It does not re-read the
// graph, re-order events, or re-render the timeline -- harness.Trace
// does all of that. This handler is a thin adapter: parse the planId
// arg, call (*TraceReader).Trace, and pack the result into one
// synthetic MemoryNode so it rides back over the standard
// integration-capability return path (a []memorynodes.MemoryNode, same
// as recall).
//
// DSL surface (dsl/harness/queries.memql -> builtin harnessTrace):
//
//	harnessTrace({ planId: "v1:harness:plan:..." })
//
// Returns a SINGLE synthetic node whose JSON payload carries:
//
//	{
//	  "planId":    "<the requested plan id>",
//	  "timeline":  "<tr.Render() -- the human-readable timeline>",
//	  "complete":  <bool -- tr.IsComplete(), plan reached terminal status>,
//	  "stepCount": <int -- len(tr.Steps)>
//	}
//
// Owner-scoping: a trace is for the caller's own plan. The harness
// mutations stamp createdBy/ownerUserId from the authenticated actor,
// and the cockpit calls this with its own credentials, so the actor in
// context IS the plan owner. We resolve that actor (mirroring recall's
// component/auth handling) and log it for the audit trail. The trace
// reader reads the plan's own version history; cross-tenant isolation
// is enforced upstream at the per-row-authz layer the same way recall
// documents it. (The reader currently filters by plan id only -- the
// SQL-level owner predicate is the integration-covered path the PR
// notes; the actor resolution here is the documented hook for it.)
package harnesstrace

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
	"github.com/znasllc-io/memql/component/harness"
	"github.com/znasllc-io/memql/component/memql"
)

// traceConcept is the synthetic concept stamped on the single returned
// node. It is not a real graph concept (this builtin reads, never
// writes) -- it just labels the envelope so a consumer inspecting the
// result knows what it is looking at.
const traceConcept = "v1:harness:trace"

// Integration holds the state the harnessTrace handler needs:
// a lazily-resolved bun handle (the trace reader reads via bun). It is
// constructed by the plug-in factory and populated via SetDBGetter at
// registration time, mirroring integrations/harnessrecall.
type Integration struct {
	Logger *slog.Logger

	dbGetter func() *bun.DB
}

// New constructs a harnessTrace integration.
func New(logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{Logger: logger}
}

// SetDBGetter injects the lazy bun handle getter. The trace assembler
// (harness.NewTraceReader) takes exactly this shape, so we hand it
// straight through -- no adapter.
func (i *Integration) SetDBGetter(f func() *bun.DB) { i.dbGetter = f }

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "harnessTrace" }

// Capabilities lists the DSL-callable functions. One capability:
// trace.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "trace",
			Description: "Fetch a harness plan's full execution timeline (every plan/step version transition + all observations, ordered by createdAt) reconstructed from the append-only graph event stream. Returns one synthetic node carrying the rendered timeline, completion flag, and step count. The history-over-gRPC contract for the cockpit `harness trace` CLI.",
			Handler:     i.traceHandler,
			ArgsSchema: map[string]string{
				"planId": "string (required) - the v1:harness:plan id whose timeline to reconstruct",
			},
		},
	}
}

// traceHandler is the single DSL-facing entry point. It resolves the
// planId arg, runs the existing harness trace assembler, and packs the
// result into one synthetic MemoryNode.
func (i *Integration) traceHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	planID, err := parsePlanID(args)
	if err != nil {
		return nil, err
	}

	if i.dbGetter == nil || i.dbGetter() == nil {
		return nil, fmt.Errorf("harnessTrace.trace: database not configured")
	}

	// Owner-scope: the trace is for the caller's own plan. Resolve the
	// authenticated actor (mirroring recall) for the audit trail. Empty
	// in dev mode / no auth.
	owner := ownerFromContext(ctx)

	reader := harness.NewTraceReader(i.dbGetter)
	tr, err := reader.Trace(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("harnessTrace.trace: assemble trace for %q: %w", planID, err)
	}

	node, err := buildTraceNode(planID, tr)
	if err != nil {
		return nil, fmt.Errorf("harnessTrace.trace: build node for %q: %w", planID, err)
	}

	i.Logger.Info("harnessTrace.trace: completed",
		"planId", planID,
		"owner", owner,
		"complete", tr.IsComplete(),
		"step_count", len(tr.Steps),
		"event_count", len(tr.Events),
	)
	return []memorynodes.MemoryNode{node}, nil
}

// parsePlanID validates the required planId arg.
func parsePlanID(args map[string]any) (string, error) {
	planID, _ := args["planId"].(string)
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return "", fmt.Errorf("harnessTrace.trace: planId is required")
	}
	return planID, nil
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
// Payload. It is the wire contract the cockpit `harness trace` CLI
// decodes.
type tracePayload struct {
	PlanID    string `json:"planId"`
	Timeline  string `json:"timeline"`
	Complete  bool   `json:"complete"`
	StepCount int    `json:"stepCount"`
}

// buildTraceNode packs an assembled Trace into the single synthetic
// MemoryNode the handler returns. The id is deterministic so repeated
// calls for the same plan return a stable node id (the trace is a
// read-through view, not a new graph row).
func buildTraceNode(planID string, tr harness.Trace) (memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(tracePayload{
		PlanID:    planID,
		Timeline:  tr.Render(),
		Complete:  tr.IsComplete(),
		StepCount: len(tr.Steps),
	})
	if err != nil {
		return memorynodes.MemoryNode{}, fmt.Errorf("marshal payload: %w", err)
	}
	return memorynodes.MemoryNode{
		ID:        "harness-trace:" + planID,
		Concept:   traceConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}, nil
}
