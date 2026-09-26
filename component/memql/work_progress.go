package memql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// WorkEvent is public execution evidence, never a model's private reasoning.
// Unknown token usage and cost remain absent, not misleading zeroes.
type WorkEvent struct {
	Text           string                   `json:"text,omitempty"`
	ID             string                   `json:"id"`
	Kind           string                   `json:"kind"`
	Phase          string                   `json:"phase"`
	At             time.Time                `json:"at"`
	Provider       string                   `json:"provider,omitempty"`
	Model          string                   `json:"model,omitempty"`
	Name           string                   `json:"name,omitempty"`
	App            string                   `json:"app,omitempty"`
	Navigate       bool                     `json:"navigate,omitempty"`
	Arguments      map[string]any           `json:"arguments,omitempty"`
	ElapsedMS      int64                    `json:"elapsedMs,omitempty"`
	ExpectedMS     int64                    `json:"expectedMs,omitempty"`
	EstimateSource string                   `json:"estimateSource,omitempty"`
	Error          string                   `json:"error,omitempty"`
	Call           *airoute.CallObservation `json:"call,omitempty"`
}

// RecordWorkProgress persists public execution evidence on the same journal
// used by every work surface. Readers need no state from the executing replica.
// Repeated IDs update a snapshot (text or model status), rather than creating
// one graph row per token. Failures propagate before another effect can run.
func (e *MemQLEngine) RecordWorkProgress(ctx context.Context, event WorkEvent) error {
	run, ok := common.RunFromContext(ctx)
	if !ok {
		return nil
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return fmt.Errorf("work progress requires the run owner's authority")
	}
	if event.ID == "" {
		event.ID = id.NewShortId()
	}
	event.At = time.Now().UTC()
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	call, err := parser.RenderCall("createWorkObservation", map[string]any{
		"observationId": "progress-" + fmt.Sprintf("%x", sha256.Sum256([]byte(run.RunId+":"+run.StepKey+":"+event.ID))),
		"runId":         run.RunId, "stepKey": run.StepKey, "kind": "note",
		"content": "Execution progress: " + event.Kind + " " + event.Name + " " + event.Phase,
		"data":    map[string]any{"execution": data},
	})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

// ObserveWorkCalls is installed on the replica that makes the calls, including
// compilation. It records the exact router metadata available at that moment.
func (e *MemQLEngine) ObserveWorkCalls(ctx context.Context, cancel context.CancelCauseFunc) context.Context {
	return airoute.WithObserver(ctx, func(call airoute.CallObservation) {
		if err := e.RecordWorkProgress(ctx, WorkEvent{ID: call.ID, Kind: "model", Phase: call.Phase, Provider: call.Provider, Model: call.Model, ElapsedMS: int64(call.ElapsedMS), Error: call.Error, Call: &call}); err != nil {
			cancel(err)
		}
	})
}
