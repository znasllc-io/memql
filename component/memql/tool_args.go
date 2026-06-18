package memql

import "context"

// applyToolDefaults merges server-provided defaults into the LLM-
// supplied tool-call args map.
//
// For fields named in tool.AutoInjectedFields the server default
// ALWAYS wins, regardless of what the LLM supplied -- and if the
// server has no default for an auto-injected field, the
// LLM-supplied value is dropped entirely. This is defense in
// depth: an LLM that hallucinates or attempts to forge values for
// fields like ownerUserId / agentId / spaceId can't sneak the
// forged value past the central validator just because the
// runtime forgot to stamp the default.
//
// MCP exception (memql#1684): when ctx carries WithMCPToolExecution
// and the server has NO default for an auto-injected field, the
// caller-supplied value is preserved instead of dropped. Over MCP
// the caller is the authenticated user (not an LLM), so a
// caller-supplied spaceId on tools like recentChat is a legitimate
// input. The security invariant is maintained: when a server
// default IS available it still wins regardless of MCP context,
// so the MCP path cannot escalate beyond what the server would
// otherwise stamp.
//
// For all other fields the legacy "fill-if-missing" semantic
// applies: defaults populate fields the LLM omitted, but don't
// overwrite fields the LLM set.
//
// Returns the (possibly mutated, possibly newly-allocated) args
// map; nil + no defaults = nil. A nil tool is treated as "no
// auto-injected fields known" so unknown-tool dispatch paths
// preserve the legacy behavior; the unknown-tool branch will
// reject the call anyway, so the validator's strictness there is
// moot.
//
// Surfaced by memql#107 (centralised tool-arg validator +
// auto-injected enforcement).
func applyToolDefaults(ctx context.Context, tool *Tool, args map[string]any, defaults map[string]any) map[string]any {
	if len(defaults) == 0 && (tool == nil || len(tool.AutoInjectedFields) == 0) {
		return args
	}
	if args == nil {
		args = make(map[string]any, len(defaults))
	}

	autoInjected := map[string]struct{}{}
	if tool != nil {
		for _, field := range tool.AutoInjectedFields {
			autoInjected[field] = struct{}{}
		}
	}

	// Pass 1: auto-injected fields -- server default wins, or drop.
	// MCP exception: when no server default is available AND the call
	// originated from the MCP connector, preserve the caller-supplied
	// value instead of dropping it (memql#1684).
	isMCP := mcpToolExecution(ctx)
	for field := range autoInjected {
		if v, ok := defaults[field]; ok {
			// Server default always wins, even on the MCP path.
			args[field] = v
		} else if !isMCP {
			// Agent runtime: drop LLM-supplied value (security).
			delete(args, field)
		}
		// else MCP + no server default: preserve caller-supplied value.
	}

	// Pass 2: every other default -- fill-if-missing.
	for k, v := range defaults {
		if _, isAuto := autoInjected[k]; isAuto {
			continue
		}
		if _, exists := args[k]; !exists {
			args[k] = v
		}
	}

	return args
}
