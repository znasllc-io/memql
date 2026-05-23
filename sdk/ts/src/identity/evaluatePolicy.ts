// evaluatePolicy wraps MemqlClientMessage.evaluate_policy. The
// handler requires the policy to be @frontend_visible + tier=bff;
// core policies (auth / partition / etc.) are reachable only from
// the engine itself, never from a browser.
//
// argsJson on the wire is a JSON-encoded string -- the SDK
// serializes the caller's plain-object args (handled here, not by
// the consumer). result_json on the reply is similarly a
// JSON-encoded string of the policy's return value; the SDK
// decodes it to `unknown` so the caller can narrow against the
// known policy shape.
//
// Typed error codes the SDK does NOT throw on (they ride the
// returned object so the caller can branch):
//   POLICY_UNKNOWN
//   POLICY_NOT_FRONTEND_VISIBLE
//   POLICY_TIER_MISMATCH
//   POLICY_RUNTIME_ERROR
// QueryError on the dispatcher path is still thrown.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface EvaluatePolicyArgs {
  policyName: string;
  // Caller-passed args; merged with ctx fields (actor / partition /
  // now) server-side. The SDK serializes this object to JSON before
  // sending; pass plain JSON-safe values.
  args?: Record<string, unknown>;
  // When true the trace tree is always included; when false the
  // trace is included only if the policy carries @returns_trace.
  returnTrace?: boolean;
  signal?: AbortSignal;
}

export interface EvaluatePolicyResult {
  // Decoded from result_json; null when no result was returned (the
  // typical error-code paths set this to null and populate the code
  // / message instead).
  result: unknown;
  // Structured PolicyTrace tree decoded from trace_json. null when
  // the engine returned no trace (either returnTrace=false AND the
  // policy lacks @returns_trace, or the policy errored before any
  // trace lines could be emitted).
  trace: unknown;
  errorCode: string;
  errorMessage: string;
}

export async function evaluatePolicy(
  dispatcher: Dispatcher,
  args: EvaluatePolicyArgs,
): Promise<EvaluatePolicyResult> {
  if (!dispatcher) throw new Error("evaluatePolicy: dispatcher is required");
  if (!args.policyName) throw new Error("evaluatePolicy: policyName is required");

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      evaluatePolicy: {
        requestId,
        policyName: args.policyName,
        argsJson: args.args ? JSON.stringify(args.args) : "",
        ...(args.returnTrace ? { returnTrace: true } : {}),
      },
    },
    args.signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`evaluatePolicy: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "evaluatePolicyResult") {
    throw new Error("evaluatePolicy: unexpected reply envelope");
  }
  return {
    result: decodeJSONField(payload.value.resultJson),
    trace: decodeJSONField(payload.value.traceJson),
    errorCode: payload.value.errorCode ?? "",
    errorMessage: payload.value.errorMessage ?? "",
  };
}

function decodeJSONField(raw: string | undefined): unknown {
  if (!raw) return null;
  try {
    return JSON.parse(raw);
  } catch {
    // The engine should never emit non-JSON here; if it does, the
    // raw string is more useful than swallowing it silently.
    return raw;
  }
}
