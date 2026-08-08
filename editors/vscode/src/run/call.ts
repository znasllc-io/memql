// Rendering a named-invocation call string for ExecuteQueryMsg.
//
// A query / mutation / logic run goes over the wire as a CALL STRING --
// `query spaceParticipants(spaceId: "abc")` -- not as a name plus a JSON
// argument map. The engine parses it, so the literal syntax has to be the
// engine's, and this file mirrors sdk/go/client/support.go's renderMemQLValue
// + quoteMemQL one-for-one rather than inventing a second dialect. The Go SDK
// generates thousands of these builders from the DSL; when the two disagree,
// the Go side is right.
//
// Deliberately free of `vscode` imports -- pure string work, tested under bare
// `node --test`.

import type { RunnableKind } from "../constructs/runnable.js";

// The CALL KEYWORD is not the construct keyword. The DSL declares a mutation
// with `mutate`, and the engine's named-call parser accepts `mutation`
// (component/memql/authoring_session.go and the generated Go builders both use
// it). Getting this mapping wrong produces a parse failure at the engine with
// no hint about which side is confused, so it lives here as one table.
const CALL_KEYWORD: Readonly<Record<string, string>> = {
  query: "query",
  mutate: "mutation",
  logic: "logic",
};

export function callKeywordFor(kind: RunnableKind): string | undefined {
  return CALL_KEYWORD[kind];
}

/**
 * buildNamedCall renders `<keyword> <name>(argName: <literal>, ...)`.
 *
 * Argument ORDER follows the declared arg list, not the object's key order:
 * the form and any run configuration are both keyed maps, and JSON object key
 * order is an implementation detail nobody should be depending on for a string
 * the engine parses.
 *
 * An argument the caller did not supply is OMITTED rather than rendered as
 * nil. The two are different to the engine -- an omitted optional arg lets the
 * body's `??` default apply, while an explicit nil is a supplied null that
 * `??` also coalesces but that a `when(args.x)` guard treats as PRESENT. The
 * form's empty box means "I did not supply this", so it must omit.
 */
export function buildNamedCall(
  kind: RunnableKind,
  name: string,
  argOrder: readonly string[],
  values: Readonly<Record<string, unknown>>,
): string {
  const keyword = callKeywordFor(kind);
  if (keyword === undefined) {
    throw new Error(
      `buildNamedCall: ${kind} is not invoked through ExecuteQueryMsg (a tool goes through CallToolMsg; an automation is memql#3310)`,
    );
  }
  if (name === "") throw new Error("buildNamedCall: name is required");

  // Any supplied key the declared order does not mention is appended after
  // it. That happens when a run configuration was authored by hand against a
  // construct whose args have since changed -- dropping the value silently
  // would run something the file does not say, so it is passed through and
  // the engine gets to reject it by name.
  const seen = new Set<string>();
  const ordered: string[] = [];
  for (const key of argOrder) {
    if (key in values && !seen.has(key)) {
      seen.add(key);
      ordered.push(key);
    }
  }
  for (const key of Object.keys(values)) {
    if (!seen.has(key)) {
      seen.add(key);
      ordered.push(key);
    }
  }

  const parts = ordered.map((key) => `${key}: ${renderMemqlValue(values[key])}`);
  return `${keyword} ${name}(${parts.join(", ")})`;
}

/**
 * renderMemqlValue converts a JSON value to its MemQL literal form.
 *
 * Mirrors sdk/go/client/support.go's renderMemQLValue: object keys are SORTED
 * so the same argument map always renders to the same string (which is what
 * lets a run configuration be diffed, and what keeps the session-define cache
 * key stable), and strings go through JSON quoting because the engine's
 * QuoteString is a JSON encoder with HTML escaping turned off.
 */
export function renderMemqlValue(value: unknown): string {
  if (value === null || value === undefined) return "nil";
  switch (typeof value) {
    case "string":
      return quoteMemql(value);
    case "boolean":
      return value ? "true" : "false";
    case "number":
      // A non-finite number has no literal form. Emitting `Infinity` or `NaN`
      // would produce a call string the engine cannot parse, with an error
      // that names the construct rather than the argument -- so it is refused
      // here, where the argument's name is still in hand at the call site.
      if (!Number.isFinite(value)) {
        throw new Error(`renderMemqlValue: ${String(value)} has no MemQL literal form`);
      }
      return String(value);
    default:
      break;
  }
  if (Array.isArray(value)) {
    return `[${value.map(renderMemqlValue).join(", ")}]`;
  }
  if (typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>).sort(([a], [b]) =>
      a < b ? -1 : a > b ? 1 : 0,
    );
    return `{${entries.map(([k, v]) => `${k}: ${renderMemqlValue(v)}`).join(", ")}}`;
  }
  throw new Error(`renderMemqlValue: unsupported value of type ${typeof value}`);
}

// quoteMemql produces the engine's string literal. JSON.stringify is the same
// encoder QuoteString wraps (component/language/parser/quote.go), minus the
// HTML escaping the Go side switches off -- and JSON.stringify does not escape
// HTML at all, so the two agree.
export function quoteMemql(s: string): string {
  return JSON.stringify(s);
}

/**
 * extractErrorId pulls the engine's short error id out of a message.
 *
 * The engine stamps `ERR-<6 hex>` (component/grpc/ai_handlers.go's
 * generateErrorId) and logs the full context under it, so the id is the only
 * thing that ties what the developer is looking at to what the operator can
 * find in the logs. Surfacing it separately -- copyable -- is the difference
 * between a support thread that resolves and one that does not.
 *
 * Empty when the message carries no id, which is the ordinary case for a
 * validation or parse failure.
 */
export function extractErrorId(message: string): string {
  const m = /\bERR-[0-9a-fA-F]{6}\b/.exec(message);
  return m === null ? "" : m[0];
}
