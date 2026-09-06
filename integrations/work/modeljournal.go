package work

// modeljournal.go -- the v1:work:modelCall writer and reader (memql#4999).
//
// THE OTHER HALF OF fork.go's RESIDUAL. forkRun and replayRun open a run whose
// `mode` says what its journal may serve, and record in full that "a run
// opened in replay mode is a row that says `replay` and nothing reads it".
// component/memql's model seam is what reads it; this is what fills it.
//
// WHY THE WRITER IS HERE RATHER THAN IN THE ENGINE. createWorkModelCall is
// @serverOnly, and the stamping discipline for the work mutations is a
// PACKAGE-level allowlist entry with exactly one stamp site, asserted by
// internal_origin_test.go. Growing a second one in component/memql would need
// a second allowlist entry and a second precondition test for one mutation.
// So the engine holds an interface and this package implements it -- which is
// also the only direction the imports can go.
//
// THE ACTOR IS BORROWED, on the write and on the read alike, for the reason
// store.go's header states at length: createWorkModelCall stamps ownerUserId
// from actor.userId, and workModelCallsForOwnerRun filters on
// ownerUserId==actor.userId. An unstamped write lands on a row readable by
// nobody; an unstamped read returns zero rows and no error, which a replay
// would take for "nothing was recorded" and -- under strict policy -- report
// as a divergence blaming the prompt.

import (
	"context"
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// ModelJournal implements memqlengine.ModelCallJournal over v1:work:modelCall.
type ModelJournal struct{ store *store }

var _ memqlengine.ModelCallJournal = (*ModelJournal)(nil)

// NewModelJournal builds the journal over an engine. Returns nil for a nil
// engine, so app/ can wire unconditionally and a binary with no engine gets a
// live-only seam rather than a panic.
func NewModelJournal(engine Engine) *ModelJournal {
	if engine == nil {
		return nil
	}
	return &ModelJournal{store: &store{engine: engine}}
}

// Lookup finds a recorded call for one request hash within one run.
//
// A MISS IS (nil, false) AND NEVER AN ERROR at the seam's contract, but a
// FAILED read is not a miss -- so a read error is logged by the caller's
// verdict rather than swallowed here... except that the interface has no error
// to return. That is deliberate and the trade is stated: DecideServe is built
// to receive a miss, and a strict replay turns a miss into a divergence that
// FAILS the run. So a transient read failure surfaces as a loud divergence
// rather than as a silent live call, which is the safe direction.
func (j *ModelJournal) Lookup(ctx context.Context, ownerUserId, runId, requestHash string) (*memqlengine.JournaledCall, bool) {
	if j == nil || j.store == nil {
		return nil, false
	}
	runId = strings.TrimSpace(runId)
	requestHash = strings.TrimSpace(requestHash)
	if runId == "" || requestHash == "" {
		return nil, false
	}
	rows, err := j.readRun(ctx, ownerUserId, runId)
	if err != nil {
		return nil, false
	}
	// FIRST MATCH IN ROW ORDER, and the order is the read's. The concept
	// declares no uniqueness on requestHash, so one run CAN hold two rows with
	// the same hash -- a step retried after a failure is the ordinary case.
	// Serving the first is what makes a replay follow the recorded run's own
	// sequence rather than its last attempt.
	for _, row := range rows {
		if rowString(row, "requestHash") != requestHash {
			continue
		}
		// A row that RECORDS AN ERROR is not an answer. Serving it would
		// return an empty response as though the provider had answered
		// emptily; the run must ask again, or diverge and say why.
		if rowString(row, "error") != "" {
			continue
		}
		return &memqlengine.JournaledCall{
			RunId:         runId,
			StepKey:       rowString(row, "stepKey"),
			RequestHash:   requestHash,
			Provider:      rowString(row, "provider"),
			Model:         rowString(row, "model"),
			PromptRef:     rowString(row, "promptRef"),
			PromptVersion: rowString(row, "promptVersion"),
			InputTokens:   rowInt(row, "inputTokens"),
			OutputTokens:  rowInt(row, "outputTokens"),
			Served:        rowString(row, "served"),
			Response:      rowMap(row, "response"),
		}, true
	}
	return nil, false
}

// readRun loads one run's journal through whichever query the run's OWNERSHIP
// makes readable.
//
// TWO QUERIES, BECAUSE THERE ARE TWO KINDS OF RUN, and picking one would make
// the journal work for half of them. A goal a person asked for is owned by
// that person, and workModelCallsForOwnerRun filters ownerUserId ==
// actor.userId. An automation's run is the DEPLOYMENT's: journalContext in
// component/automations writes it under a Synthetic cluster actor, and
// undoNonPrincipalOwnerStamp blanks the owner -- so the owner-scoped query
// matches nothing at all, forever, and a strict replay of an automation would
// report a divergence blaming the prompt.
//
// The blank-owner branch is NOT a widening. workModelCallsForRun is
// @serverOnly and carries `actor.isClusterOwner==true`, so it opens only
// under the same synthetic cluster actor that WROTE those rows; a caller
// context without it reads zero rows exactly as before.
func (j *ModelJournal) readRun(ctx context.Context, ownerUserId, runId string) ([]map[string]any, error) {
	if strings.TrimSpace(ownerUserId) != "" {
		return j.store.query(ownerActor(ctx, ownerUserId),
			"query "+call("workModelCallsForOwnerRun", map[string]any{"runId": runId}))
	}
	return j.store.queryInternal(ctx,
		"query "+call("workModelCallsForRun", map[string]any{"runId": runId}))
}

// Record writes one v1:work:modelCall row.
func (j *ModelJournal) Record(ctx context.Context, ownerUserId string, c memqlengine.JournaledCall) error {
	if j == nil || j.store == nil {
		return nil
	}
	return j.store.writeInternal(ownerActor(ctx, ownerUserId), "mutation "+call("createWorkModelCall", map[string]any{
		"modelCallId":   newRowId(concept.ConceptWorkModelCall),
		"runId":         c.RunId,
		"stepKey":       c.StepKey,
		"requestHash":   c.RequestHash,
		"provider":      c.Provider,
		"model":         c.Model,
		"settings":      optMap(c.Settings),
		"promptRef":     c.PromptRef,
		"promptVersion": c.PromptVersion,
		"inputTokens":   c.InputTokens,
		"outputTokens":  c.OutputTokens,
		"cost":          c.Cost,
		"latencyMs":     c.LatencyMs,
		"served":        c.Served,
		"response":      optMap(c.Response),
		"error":         c.Error,
	}))
}

// rowInt reads a numeric field.
//
// Decoded payload numbers arrive as float64, and a bare int(x) out of range is
// implementation-defined -- on amd64 it answers with the integer indefinite
// value, so a huge count becomes hugely negative. core/num is the ONE
// narrowing, and the answer named here is SATURATE: a token count is a
// magnitude, and a wrapped negative one would make a run's spend read as a
// refund.
func rowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case float64:
		return num.ClampFloat64(v)
	case int64:
		return num.ClampInt64(v)
	case int:
		return v
	default:
		return 0
	}
}
