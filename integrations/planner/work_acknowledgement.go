package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

type acknowledgementCandidateKey struct{}
type classificationViewerKey struct{}

func validAcknowledgement(text string) bool {
	return strings.TrimSpace(text) != "" && len([]rune(text)) <= 280
}

func (l *PlannerAgentLoop) repairAcknowledgement(ctx context.Context, req CompileRequest, conversation any, out *CompileOutcome) string {
	data := map[string]any{"goal": truncate(req.Statement, maxGoalChars), "workload": out.Workload, "workTitle": out.WorkTitle}
	if conversation != nil {
		if preview, err := conversationPreview(conversation); err == nil {
			data["conversation"] = preview
		}
	}
	bounded, cancel := context.WithTimeout(airoute.WithCallPurpose(ctx, "Acknowledging request", 1), 15*time.Second)
	defer cancel()
	response, err := l.engine.InvokeAI(systemActorContext(bounded), "workAcknowledgement", data)
	if err == nil || !memql.IsProviderUnavailable(err) {
		out.ModelCalls++
	}
	if err == nil {
		text := strings.TrimSpace(parseSectionableDecision(response).Acknowledgement)
		if validAcknowledgement(text) {
			return text
		}
		err = fmt.Errorf("model returned an empty or oversized acknowledgment")
	}
	// Useful work may proceed after this bounded presentation failure. Ask
	// renders the real task status, never an empty assistant message or canned
	// prose, and the failed model call remains in the execution record.
	l.warnCompile("work compile: acknowledgment unavailable after one repair", req, err)
	return ""
}

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
