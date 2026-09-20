package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/mcp"
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
// ONE ALLOCATOR, AND IT IS component/worker's
// ============================================================================
// A run's step order is what a client sorts by, and two writers cannot
// produce one order without sharing a counter. `ClaimRecordingSlot` is that
// shared read-modify-write, and it answers all three things this side needs
// -- the owner, the recording run and the position -- from one read of the
// row. Writing a second copy here is what would drift: it would be the same
// three lines in two packages, and the first divergence is a timeline whose
// steps sit on top of each other.

// mcpAppSessionRecorder writes an app's MCP tool calls into its recording.
type mcpAppSessionRecorder struct {
	sessions *workerservice.EngineStore
	writer   *work.SessionWriter
	logger   *slog.Logger
}

var _ mcp.AppSessionRecorder = (*mcpAppSessionRecorder)(nil)

func newMCPAppSessionRecorder(a *App) *mcpAppSessionRecorder {
	if a == nil || a.engine == nil {
		return nil
	}
	return &mcpAppSessionRecorder{
		sessions: &workerservice.EngineStore{Engine: a.engine, Logger: a.Logger},
		writer:   work.NewSessionWriter(a.engine, a.Logger),
		logger:   a.Logger,
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
	// NO OWNER IS PASSED: an app-session credential's subject IS the owning
	// user, so the inbound actor is already the right one and there is
	// nothing to borrow. That is the whole reason the class exists
	// (memql#4857), and it is what makes this read safe without a wider
	// authority.
	slot, err := r.sessions.ClaimRecordingSlot(ctx, sessionId, "")
	if err != nil {
		// A session this credential cannot read is not this caller's, which
		// the credential makes impossible unless the row was deleted
		// underneath it. Logged rather than raised: the tool already ran and
		// its result is on its way back to the app.
		r.logger.Warn("app session recording: the session's recording slot could not be claimed; the MCP call is unrecorded",
			"session_id", sessionId, "error", err)
		return err
	}
	if strings.TrimSpace(slot.OwnerUserId) == "" || strings.TrimSpace(slot.RunId) == "" {
		// A session opened before the recording existed, or on a node with no
		// recorder. Nothing to write into, and inventing a run would put a
		// step somewhere no reader of this session will look.
		return nil
	}

	return r.writer.RecordAction(ctx, workerservice.RecordedAction{
		SessionId:   sessionId,
		OwnerUserId: slot.OwnerUserId,
		RunId:       slot.RunId,
		Seq:         slot.Seq,
		Action: workerservice.ActionEvent{
			// THE STEP KEY DERIVES FROM THIS ID, so it must be unique per
			// call within the session. The seq alone would collide with an
			// action's on a tie, and two steps at one row id is one step
			// overwriting the other.
			Id: fmt.Sprintf("mcp-%s-%d", sanitizeStepIdPart(c.Tool), slot.Seq),
			// The action seq is the CLAIMED one. It is not the cockpit's
			// counter -- the cockpit never saw this call -- and sharing that
			// space is what puts the two writers in one order.
			Seq:          uint64(slot.Seq),
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
