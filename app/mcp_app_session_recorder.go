package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/mcp"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/integrations/work"
)

// mcp_app_session_recorder.go -- the app-side half of memql#5399.
//
// ============================================================================
// THE HOP THAT IS NOT A HOP
// ============================================================================
// The session runs on an agent replica and this is the MCP node. The call the
// app just made has to land as a step of the SAME recording run the actions
// land in, and the two replicas never speak: the SESSION ROW is the state both
// can see, which is the reasoning `submit` already rests on. So this reads the
// row, takes the owner and the recording run off it, and writes through the
// same integrations/work writer the other replica uses.
//
// A NodeService forward would have the failure mode of a call the app made
// that nobody recorded, on the node that was the only one to see it.
//
// ============================================================================
// SEQ IS ALLOCATED FROM THE ROW
// ============================================================================
// A run's step order is what a client sorts by, and two writers cannot
// produce one order without sharing a counter. `recordedSteps` is that
// counter: this reads it, takes it as the step's seq, and writes it back
// incremented. A race with the other replica gives two steps one seq -- a tie
// in display order, never a lost row, because the step KEY is what the row id
// derives from and this one's is unique per call.

// mcpAppSessionRecorder writes an app's MCP tool calls into its recording.
type mcpAppSessionRecorder struct {
	engine *memqlengine.MemQLEngine
	writer *work.SessionWriter
	logger *slog.Logger
}

var _ mcp.AppSessionRecorder = (*mcpAppSessionRecorder)(nil)

func newMCPAppSessionRecorder(a *App) *mcpAppSessionRecorder {
	if a == nil || a.engine == nil {
		return nil
	}
	return &mcpAppSessionRecorder{
		engine: a.engine,
		writer: work.NewSessionWriter(a.engine, a.Logger),
		logger: a.Logger,
	}
}

// RecordToolCall writes one MCP call as an `mcp` step.
func (r *mcpAppSessionRecorder) RecordToolCall(ctx context.Context, c mcp.AppSessionToolCall) error {
	if r == nil || r.writer == nil {
		return nil
	}
	sessionId := strings.TrimSpace(c.SessionId)
	if sessionId == "" {
		return nil
	}
	session, err := r.readSession(ctx, sessionId)
	if err != nil {
		// A session this credential cannot read is not this caller's, which
		// the credential makes impossible unless the row was deleted
		// underneath it. Logged rather than raised: the tool already ran.
		r.logger.Warn("app session recording: the session row could not be read; the MCP call is unrecorded",
			"session_id", sessionId, "error", err)
		return err
	}
	owner, _ := session["ownerUserId"].(string)
	runId, _ := session["sessionRunId"].(string)
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(runId) == "" {
		// A session opened before the recording existed, or on a node with no
		// recorder. Nothing to write into, and inventing a run would put a
		// step somewhere no reader of this session will look.
		return nil
	}
	seq := r.allocateSeq(ctx, sessionId, session)

	return r.writer.RecordAction(ctx, workerservice.RecordedAction{
		SessionId:   sessionId,
		OwnerUserId: owner,
		RunId:       runId,
		Seq:         seq,
		Action: workerservice.ActionEvent{
			// THE STEP KEY DERIVES FROM THIS ID, so it must be unique per
			// call within the session. The seq alone would collide with an
			// action's on a tie, and two steps at one row id is one step
			// overwriting the other.
			Id: fmt.Sprintf("mcp-%s-%d", sanitizeStepIdPart(c.Tool), seq),
			// The action seq is the allocated one. It is not the cockpit's
			// counter -- the cockpit never saw this call -- and reusing that
			// space is what puts the two writers in one order.
			Seq:          uint64(seq),
			Tool:         "mcp",
			Args:         mcpCallArgs(c),
			ResultDigest: c.ResultDigest,
			ResultType:   c.ResultType,
			IsError:      c.IsError,
			Error:        c.Error,
		},
	})
}

// mcpCallArgs keeps the tool's NAME beside its arguments.
//
// `tool` is "mcp" on the step, because that is the step TYPE a reader filters
// on; which MemQL tool was called is a fact about the call and belongs in the
// evidence. Folding the name into the step type instead would give the spine
// one type per tool and make `mcp` unfilterable.
func mcpCallArgs(c mcp.AppSessionToolCall) map[string]any {
	args := map[string]any{"tool": c.Tool}
	if len(c.Args) > 0 {
		args["arguments"] = c.Args
	}
	return args
}

// readSession reads the app session's own row.
//
// UNDER THE SESSION'S OWNER, borrowed from... nothing yet: the row must be
// read before the owner is known. `appSessionById` is @actor and owner-tiered,
// so this runs it under the MCP request's own actor -- which for an
// app-session bearer IS the owning user, because the credential's `sub` is
// theirs. That is the whole reason the class exists (memql#4857), and it is
// what makes this read safe without a wider authority.
func (r *mcpAppSessionRecorder) readSession(ctx context.Context, sessionId string) (map[string]any, error) {
	query, err := langparser.RenderCall("appSessionById", map[string]any{"sessionId": sessionId})
	if err != nil {
		return nil, err
	}
	res, err := r.engine.Execute(ctx, "query "+query)
	if err != nil {
		return nil, err
	}
	rows, _ := res.OutputPayload().([]any)
	for _, raw := range rows {
		if row, ok := raw.(map[string]any); ok {
			return row, nil
		}
	}
	return nil, fmt.Errorf("no session row is readable for this credential")
}

// allocateSeq takes the next step position and writes it back.
//
// The write is @serverOnly and this package is on the internal-origin
// allowlist; the stamp is inline as the Execute argument, so the marked
// context dies there. A failed write-back does NOT fail the recording: the
// step still lands, and the cost is that the next allocation repeats this seq
// -- a display tie rather than a lost row.
func (r *mcpAppSessionRecorder) allocateSeq(ctx context.Context, sessionId string, session map[string]any) int {
	seq := intFromRow(session, "recordedSteps")
	query, err := langparser.RenderCall("recordAppSessionProgress", map[string]any{
		"sessionId":     sessionId,
		"recordedSteps": seq + 1,
	})
	if err != nil {
		return seq
	}
	if _, err := r.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+query); err != nil {
		r.logger.Warn("app session recording: could not advance the step allocator; the next step may repeat this seq",
			"session_id", sessionId, "error", err)
	}
	return seq
}

func intFromRow(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// sanitizeStepIdPart bounds a tool name for use inside a step key.
func sanitizeStepIdPart(name string) string {
	cleaned := strings.Map(func(rn rune) rune {
		switch {
		case rn >= 'a' && rn <= 'z', rn >= 'A' && rn <= 'Z', rn >= '0' && rn <= '9', rn == '_', rn == '-':
			return rn
		}
		return '-'
	}, strings.TrimSpace(name))
	if cleaned == "" {
		return "tool"
	}
	if len(cleaned) > 48 {
		cleaned = cleaned[:48]
	}
	return cleaned
}
