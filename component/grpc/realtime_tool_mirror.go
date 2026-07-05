package memql

import (
	"encoding/json"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// realtimeMirrorAllowlist is the low-risk read-tool set whose DIRECT-path
// (browser) CallTool invocations we mirror into a structured awareness
// breadcrumb (frontend#158). It mirrors the relay bridge's
// `lowRiskToolAllowlist` in integrations/voice/agent/mcp_tool_bridge.go --
// kept as a small local copy because component/grpc must not depend on the
// voice-agent integration package. Both case styles are listed because the
// registry exposes either depending on the tool.
var realtimeMirrorAllowlist = map[string]struct{}{
	"webSearch":        {},
	"web_search":       {},
	"knowledgeLookup":  {},
	"knowledge_lookup": {},
	"domainLookup":     {},
	"domain_lookup":    {},
	"recentChat":       {},
	"recent_chat":      {},
}

// isRealtimeMirroredTool reports whether a tool name is in the low-risk
// realtime allowlist we mirror.
func isRealtimeMirroredTool(name string) bool {
	_, ok := realtimeMirrorAllowlist[strings.TrimSpace(name)]
	return ok
}

// flattenToolResultContent joins the text content parts of a CallTool result
// into one string (matching the relay bridge's flattenToolResultText).
func flattenToolResultContent(content []*memqlv1.ToolResultContent) string {
	var parts []string
	for _, item := range content {
		if item == nil {
			continue
		}
		if t := item.GetText(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// mirrorRealtimeToolCall emits a structured voiceTrace breadcrumb for a
// model-driven low-risk read-tool call on the DIRECT browser path, matching
// the relay's NewLogMirrorSink so the two voice paths have the same
// cognition-awareness footprint (frontend#158).
//
// It is a no-op when:
//   - this is a voice-agent stream (the relay self-mirrors in-process, so a
//     second breadcrumb here would double-log the same call), or
//   - the tool is not on the low-risk realtime allowlist (we only mirror the
//     read tools the direct session exposes; other CallTool callers -- text
//     chat, recipes -- are out of scope).
//
// Best-effort and side-effect-free beyond the log line: it never fails or
// delays the call the model is awaiting.
func (s *streamSession) mirrorRealtimeToolCall(
	callerRole, toolName string,
	args *structpb.Struct,
	resultText string,
	isError bool,
) {
	if s == nil || s.logger == nil {
		return
	}
	if s.voiceAgentScopeId != "" {
		return
	}
	if !isRealtimeMirroredTool(toolName) {
		return
	}

	argsJSON := "{}"
	if args != nil {
		// encoding/json marshals map keys in sorted order, so the breadcrumb
		// is deterministic.
		if b, err := json.Marshal(args.AsMap()); err == nil {
			argsJSON = string(b)
		}
	}

	result := resultText
	if len(result) > 500 {
		result = result[:500]
	}

	s.logger.Info("voice trace: realtime mcp tool call",
		"stage", "realtime.mcp.tool.call",
		"component", ComponentName,
		"caller_role", callerRole,
		"caller_subject", s.identity.Subject,
		"tool", strings.TrimSpace(toolName),
		"is_error", isError,
		"arguments", argsJSON,
		"result", result)
}
