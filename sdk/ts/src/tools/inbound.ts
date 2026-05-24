// Inbound ClientToolCall dispatch. Mirrors sdk/go/client/tools.go
// RegisterClientToolHandler (memql#174). When the server's agent
// loop resolves a tool marked client_execution=true it pushes a
// ClientToolCall envelope; the consumer handler runs locally and
// the SDK ships the returned result back as a ClientToolResult
// correlated by callId.
//
// One handler at a time per dispatcher. Re-register replaces; the
// first call's unregister becomes a no-op (version-stamped to its
// own handler).
//
// The consumer handler is called with a per-call AbortSignal that
// fires after the call's `timeoutMs` budget elapses, so a long-
// running handler can cooperatively cancel rather than missing the
// server's deadline. A handler that throws or returns falsy ships
// back isError=true so the agent's parked tool loop always
// unblocks.

import type { Dispatcher } from "../client/dispatcher.js";
import {
  readServerPayload,
  type ToolResultContentWire,
} from "../client/wire.js";
import { contentFromWire, type ToolResultContent } from "./outbound.js";

export interface ClientToolCall {
  callId: string;
  turnId: string;
  agentId: string;
  toolName: string;
  // Raw JSON string -- per-tool handlers unmarshal per their own
  // schema (the server side validates against ToolDefinition's
  // inputSchema, but the SDK doesn't enforce shape here).
  argumentsJson: string;
  // Server-side deadline (ms). 0 / negative means "no deadline";
  // the SDK arms the AbortSignal only when > 0.
  timeoutMs: number;
}

export interface ClientToolResult {
  content?: ToolResultContent[];
  isError?: boolean;
  errorMessage?: string;
}

export type ClientToolHandler = (
  call: ClientToolCall,
  signal: AbortSignal,
) => Promise<ClientToolResult> | ClientToolResult;

export type ClientToolHandlerUnregister = () => void;

// Module-level registry keyed by Dispatcher instance. Each dispatcher
// gets at most one event-listener wrapper that fans inbound
// ClientToolCall envelopes to the currently-registered handler.
// Re-register replaces the slot but does NOT detach the wrapper
// (cheaper than tearing it down + re-attaching on every swap).
interface Slot {
  handler: ClientToolHandler | null;
  version: number;
  detach: (() => void) | null;
}
const slots = new WeakMap<Dispatcher, Slot>();

export function registerClientToolHandler(
  dispatcher: Dispatcher,
  handler: ClientToolHandler,
): ClientToolHandlerUnregister {
  if (!dispatcher) throw new Error("registerClientToolHandler: dispatcher is required");
  if (!handler) throw new Error("registerClientToolHandler: handler is required");

  let slot = slots.get(dispatcher);
  if (!slot) {
    slot = { handler: null, version: 0, detach: null };
    slots.set(dispatcher, slot);
    // Attach the event-listener wrapper lazily on first Register so
    // a Dispatcher that never touches client tools pays no fanout
    // cost.
    slot.detach = dispatcher.addEventListener((msg) => {
      const s = slots.get(dispatcher);
      if (!s || !s.handler) return;
      const payload = readServerPayload(msg);
      if (payload?.kind !== "clientToolCall") return;
      void runHandler(dispatcher, s.handler, payload.value);
    });
  }
  slot.version += 1;
  slot.handler = handler;
  const myVersion = slot.version;

  let consumed = false;
  return () => {
    if (consumed) return;
    consumed = true;
    const cur = slots.get(dispatcher);
    if (!cur || cur.version !== myVersion) return;
    cur.handler = null;
    // We deliberately do NOT detach the event listener -- a future
    // Register reuses the same slot. The handler-null guard above
    // skips dispatch in the meantime.
  };
}

async function runHandler(
  dispatcher: Dispatcher,
  handler: ClientToolHandler,
  call: { callId: string; turnId?: string; agentId?: string; toolName: string; argumentsJson?: string; timeoutMs?: number },
): Promise<void> {
  const sdkCall: ClientToolCall = {
    callId: call.callId,
    turnId: call.turnId ?? "",
    agentId: call.agentId ?? "",
    toolName: call.toolName,
    argumentsJson: call.argumentsJson ?? "",
    timeoutMs: typeof call.timeoutMs === "number" ? call.timeoutMs : 0,
  };

  const ac = new AbortController();
  let timer: ReturnType<typeof setTimeout> | null = null;
  if (sdkCall.timeoutMs > 0) {
    timer = setTimeout(() => ac.abort(), sdkCall.timeoutMs);
  }

  let result: ClientToolResult;
  try {
    const ret = await handler(sdkCall, ac.signal);
    result = ret ?? { isError: true, errorMessage: "client tool handler returned null" };
  } catch (err) {
    result = {
      isError: true,
      errorMessage: err instanceof Error ? err.message : String(err ?? "client tool handler threw"),
    };
  } finally {
    if (timer) clearTimeout(timer);
  }

  // Ship the result. Best-effort -- if the send fails the server
  // will time out, surfaced as the same path as a silent client.
  try {
    dispatcher.send({
      clientToolResult: {
        callId: sdkCall.callId,
        content: (result.content ?? []).map(contentToWire),
        isError: result.isError === true,
        ...(result.errorMessage ? { errorMessage: result.errorMessage } : {}),
      },
    });
  } catch {
    /* swallow: nothing useful to do; the server will time out. */
  }
}

function contentToWire(c: ToolResultContent): ToolResultContentWire {
  return {
    type: c.type,
    ...(c.text ? { text: c.text } : {}),
    ...(c.mimeType ? { mimeType: c.mimeType } : {}),
    ...(c.data ? { data: c.data } : {}),
    ...(c.uri ? { uri: c.uri } : {}),
  };
}
