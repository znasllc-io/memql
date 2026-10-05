package planner

import (
	"context"
	"encoding/json"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

type acknowledgementCandidateKey struct{}
type classificationViewerKey struct{}

func (l *PlannerAgentLoop) cacheAcknowledgement(ctx context.Context, owner, run, action string) []map[string]any {
	// Reuse is optional and bounded. A missing embedding provider or a cold
	// index cannot stall the cheap classifier or turn an accepted run into failure.
	limit := 2 * time.Second
	if action == "store" {
		limit = 5 * time.Second
	}
	bounded, cancel := context.WithTimeout(ownerActorContext(ctx, owner), limit)
	defer cancel()
	call, err := parser.RenderCall("memory.workAcknowledgementCache", map[string]any{"runId": run, "action": action})
	if err != nil {
		return nil
	}
	result, err := l.engine.Execute(bounded, "builtin "+call)
	if err != nil {
		return nil
	}
	return memql.MaterializeRows(result)
}

func (l *PlannerAgentLoop) withAcknowledgementCandidate(ctx context.Context, req CompileRequest) context.Context {
	viewerCtx, cancel := context.WithTimeout(ownerActorContext(ctx, req.OwnerUserId), 2*time.Second)
	viewer, err := l.engine.Execute(viewerCtx, "builtin work.workViewerContext()")
	cancel()
	if err == nil {
		if rows := memql.MaterializeRows(viewer); len(rows) == 1 {
			if raw, err := json.Marshal(rows[0]); err == nil {
				ctx = context.WithValue(ctx, classificationViewerKey{}, string(raw))
			}
		}
	}
	rows := l.cacheAcknowledgement(ctx, req.OwnerUserId, req.RunId, "lookup")
	if len(rows) != 1 || rows[0]["acknowledgement"] == nil {
		return ctx
	}
	raw, err := json.Marshal(rows[0])
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, acknowledgementCandidateKey{}, string(raw))
}
