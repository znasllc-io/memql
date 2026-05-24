// Outbound MCP-shaped tool RPCs: listTools (enumerate) + callTool
// (invoke). Mirrors sdk/go/client/tools.go (memql#174). Both are
// request/reply via sendAndWait correlated by messageId.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import {
  readServerPayload,
  type ToolDefinitionWire,
  type ToolResultContentWire,
} from "../client/wire.js";

export interface ToolDefinition {
  name: string;
  description: string;
  inputSchema: string; // JSON Schema (raw string per MCP)
  clientExecution: boolean;
  scopes: string[];
}

export interface ListToolsArgs {
  cursor?: string;
  signal?: AbortSignal;
}

export interface ListToolsResult {
  tools: ToolDefinition[];
  nextCursor: string;
}

export async function listTools(
  dispatcher: Dispatcher,
  args: ListToolsArgs = {},
): Promise<ListToolsResult> {
  if (!dispatcher) throw new Error("listTools: dispatcher is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      listTools: {
        requestId,
        ...(args.cursor ? { cursor: args.cursor } : {}),
      },
    },
    args.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`listTools: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "listToolsResult") {
    throw new Error("listTools: unexpected reply envelope");
  }
  return {
    tools: (payload.value.tools ?? []).map(toolFromWire),
    nextCursor: payload.value.nextCursor ?? "",
  };
}

export interface CallToolArgs {
  name: string;
  // JSON-serialisable arguments. Marshalled into the proto Struct
  // envelope at the boundary; pass any JSON-safe map.
  arguments?: Record<string, unknown>;
  signal?: AbortSignal;
}

export interface ToolResultContent {
  type: string; // "text" | "image" | "resource"
  text: string;
  mimeType: string;
  data: string;
  uri: string;
}

export interface CallToolResult {
  content: ToolResultContent[];
  isError: boolean;
}

export async function callTool(
  dispatcher: Dispatcher,
  args: CallToolArgs,
): Promise<CallToolResult> {
  if (!dispatcher) throw new Error("callTool: dispatcher is required");
  if (!args.name) throw new Error("callTool: name is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      callTool: {
        requestId,
        name: args.name,
        arguments: args.arguments ?? {},
      },
    },
    args.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`callTool(${args.name}): ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "callToolResult") {
    throw new Error(`callTool(${args.name}): unexpected reply envelope`);
  }
  return {
    content: (payload.value.content ?? []).map(contentFromWire),
    isError: payload.value.isError === true,
  };
}

function toolFromWire(t: ToolDefinitionWire): ToolDefinition {
  return {
    name: t.name ?? "",
    description: t.description ?? "",
    inputSchema: t.inputSchema ?? "",
    clientExecution: t.clientExecution === true,
    scopes: t.scopes ?? [],
  };
}

export function contentFromWire(c: ToolResultContentWire): ToolResultContent {
  return {
    type: c.type ?? "",
    text: c.text ?? "",
    mimeType: c.mimeType ?? "",
    data: c.data ?? "",
    uri: c.uri ?? "",
  };
}
