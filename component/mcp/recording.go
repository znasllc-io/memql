package mcp

// recording.go -- an app's calls back into MemQL are part of its recording
// (epic memql#5396, task memql#5399, design D12).
//
// ============================================================================
// WHY THE MCP NODE RECORDS AT ALL
// ============================================================================
// A delegated app reaches MemQL over MCP with a per-run credential. Everything
// else it does -- a command, a file read, a fetch -- is reported by the
// cockpit as a normalized action event and recorded on the replica holding the
// session. Its calls back into MemQL are not: the harness sees an opaque MCP
// result and has no arguments or result to report, and the session's replica
// never sees the call at all. So the node that DOES see it records it, and the
// app's own use of MemQL stops being the one hole in the account of what it
// did.
//
// ============================================================================
// THE OWNER AND THE RUN COME FROM THE CREDENTIAL, NEVER FROM THE CALL
// ============================================================================
// The bearer's label names the session (AppSessionFromClaims), the session row
// names its owner and its recording run, and this reads both off that row. No
// tool argument names any of them -- a surface that does not offer the question
// cannot be asked it -- and the row itself is owner-tiered, so a credential
// that is not the owner's reads nothing.
//
// ============================================================================
// submit AND next_task ARE EXCLUDED
// ============================================================================
// They are the PROTOCOL, not the work: `next_task` asks what the session was
// opened to do and `submit` hands the answer back. Recording them would put
// two steps in every session's timeline that describe the session's own
// plumbing, and a procedure lifted from that timeline would try to replay
// them.
//
// ============================================================================
// NOTHING HERE IS STAMPED WITH INTERNAL ORIGIN
// ============================================================================
// This package is an HTTP and stdio server: every context descends from an
// inbound request, which is the case component/auth's allowlist names as NEVER.
// The recorder is an interface implemented in the main module, where the stamp
// is allowlisted, and this side only resolves the session and hands over what
// it saw.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

// AppSessionRecorder records one MCP tool call as a step of the session's
// recording run.
//
// Declared here and implemented in app/ over integrations/work's SessionWriter
// -- the SAME writer the replica holding the session uses, which is what keeps
// one account of what the app did rather than two that can disagree about the
// order.
type AppSessionRecorder interface {
	// RecordToolCall writes the call as an `mcp` step. It resolves the
	// session's owner and recording run itself, from the session id.
	//
	// A FAILURE IS NEVER THE CALLER'S PROBLEM. The tool already ran and its
	// result is on its way back to the app; refusing the call because the
	// recording did not land would break the app's work to protect a record
	// of it.
	RecordToolCall(ctx context.Context, call AppSessionToolCall) error
}

// AppSessionToolCall is one call to record.
type AppSessionToolCall struct {
	// SessionId comes from the bearer's own label.
	SessionId string
	// Tool is the tool's name as the app called it.
	Tool string
	// Args are the call's arguments, WHOLE. An action is only reproducible
	// from its arguments, and a shortened one reproduces a different call.
	Args map[string]any
	// ResultDigest and ResultType stand in for the result, the way an
	// action's do: the spine stays about actions rather than filling with
	// output nobody replays.
	ResultDigest string
	ResultType   string
	IsError      bool
	// Error is the failure in the tool's own words, when there was one.
	Error string
}

// recorderKey carries the recorder on the request context, the way the
// automation runner is carried.
type recorderCtxKey struct{}

func withMCPAppSessionRecorder(ctx context.Context, r AppSessionRecorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderCtxKey{}, r)
}

func appSessionRecorderFromContext(ctx context.Context) AppSessionRecorder {
	r, _ := ctx.Value(recorderCtxKey{}).(AppSessionRecorder)
	return r
}

// recordAppSessionToolCall records one dispatched tool call, when this
// connection IS a delegated app session and the tool is work rather than
// protocol.
//
// It takes the tool RESULT it is given rather than re-running anything: the
// digest is over what the app actually received, which is the only thing a
// later comparison can be about.
func recordAppSessionToolCall(ctx context.Context, appSessionId, name string, args map[string]any, result map[string]any) {
	if strings.TrimSpace(appSessionId) == "" {
		// Every other caller -- a browser, a PAT, a service account. There is
		// no session to record into, and inventing one would put a step on
		// somebody else's run.
		return
	}
	if isAppSessionTool(name) {
		return
	}

	recorder := appSessionRecorderFromContext(ctx)
	if recorder == nil {
		return
	}
	digest, kind := digestToolResult(result)
	// A DETACHED CONTEXT. The caller's is the request's and is about to end
	// with the response; a recording that raced the reply would be dropped
	// exactly on the calls that took longest.
	_ = recorder.RecordToolCall(context.WithoutCancel(ctx), AppSessionToolCall{
		SessionId:    appSessionId,
		Tool:         name,
		Args:         args,
		ResultDigest: digest,
		ResultType:   kind,
		IsError:      result["isError"] == true,
		Error:        toolResultText(result, result["isError"] == true),
	})
}

// digestToolResult reduces an MCP tool result to a digest and an inferred
// type.
//
// THE DIGEST IS OVER THE RENDERED RESULT, which is what the app received. It
// is deliberately NOT a cryptographic commitment anybody verifies -- it exists
// so two recordings of the same call can be compared, which is what a shadow
// replay does, and core/id's fingerprint is the repository's answer for that.
func digestToolResult(result map[string]any) (digest, kind string) {
	if result == nil {
		return "", ""
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return "", "unknown"
	}
	return string(id.NewUntracked().FromBytes(raw)), inferResultType(result)
}

// inferResultType names the SHAPE of what came back, which is what a later
// comparison can hold a replay to when the successful recordings themselves
// varied (design D15's "by inferred type").
func inferResultType(result map[string]any) string {
	trimmed := strings.TrimSpace(firstContentText(result))
	switch {
	case trimmed == "":
		return "empty"
	case strings.HasPrefix(trimmed, "{"):
		return "object"
	case strings.HasPrefix(trimmed, "["):
		return "array"
	}
	return "string"
}

// firstContentText reads the first content block's text.
//
// IT HANDLES BOTH SHAPES A RESULT ARRIVES IN, which is not defensiveness: a
// result composed in process is []map[string]any (textResult, errorResult)
// and one that came back through toolResultFromJSON's unmarshal is []any.
// Reading only the second answered "empty" for every in-process result, which
// is the wrong answer on the majority of calls and reads as a tool that
// returned nothing.
func firstContentText(result map[string]any) string {
	if result == nil {
		return ""
	}
	switch content := result["content"].(type) {
	case []map[string]any:
		if len(content) > 0 {
			text, _ := content[0]["text"].(string)
			return text
		}
	case []any:
		if len(content) > 0 {
			first, _ := content[0].(map[string]any)
			text, _ := first["text"].(string)
			return text
		}
	}
	return ""
}

// toolResultText pulls the text out of an MCP tool result, for the error
// field. Empty unless the call failed -- a successful result's text is the
// content the digest stands for, and copying it onto the row would put the
// output back in the spine that D5 keeps it out of.
func toolResultText(result map[string]any, isError bool) string {
	if !isError || result == nil {
		return ""
	}
	text := firstContentText(result)
	const max = 500
	if len(text) > max {
		return text[:max] + "..."
	}
	return text
}
