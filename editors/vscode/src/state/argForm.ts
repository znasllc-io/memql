// The argument form's model: fields generated from the construct's own `args`
// block, and the coercion from what the user typed back to JSON values.
//
// EVERY FIELD COMES FROM THE LANGUAGE SERVER. There is no hand-maintained
// schema anywhere in the extension, and no fallback that guesses at an
// argument the server did not report -- one parser, so the form cannot
// disagree with the compiler about what a construct accepts.
//
// The coercion is where a generated form usually goes wrong, and the rule it
// follows is: NEVER GUESS. A number field whose text is not a number is an
// error the user is shown, not a silent 0 and not a string passed through.
// The call string is parsed by the engine, so a wrongly-typed argument
// produces an engine-side error naming the construct rather than the field --
// which is a far worse place to find out.
//
// Deliberately free of `vscode` imports; webview/runPanel.ts renders these.
// Tested under bare `node --test`.

import type { RunnableArg } from "../constructs/runnable.js";

export interface ArgFieldModel {
  name: string;
  type: RunnableArg["type"];
  required: boolean;
  /** Non-empty when the arg declares a closed value set; the form renders a dropdown. */
  enumValues: string[];
  description: string;
  /** The raw text to show in the input, already rendered from any prior value. */
  text: string;
}

/**
 * buildFields generates the form model.
 *
 * `values` is a previously-filled set (from a run configuration, or from the
 * last run in this session). A value for an argument the construct NO LONGER
 * DECLARES is dropped here rather than carried into a hidden field: the form
 * shows what the construct accepts, and a value nothing on screen accounts for
 * is worse than a lost one. buildOrphanedValues names them so the caller can
 * say what happened.
 */
export function buildFields(
  args: readonly RunnableArg[],
  values: Readonly<Record<string, unknown>> = {},
): ArgFieldModel[] {
  return args.map((arg) => ({
    name: arg.name,
    type: arg.type,
    required: arg.required,
    enumValues: arg.enum ?? [],
    description: arg.description ?? "",
    text: arg.name in values ? renderValueForInput(values[arg.name], arg.type) : "",
  }));
}

/** orphanedValueNames lists supplied keys the construct does not declare. */
export function orphanedValueNames(
  args: readonly RunnableArg[],
  values: Readonly<Record<string, unknown>>,
): string[] {
  const declared = new Set(args.map((a) => a.name));
  return Object.keys(values).filter((k) => !declared.has(k));
}

// renderValueForInput turns a stored JSON value back into editable text.
// Scalars render bare (an input box showing "abc" rather than "\"abc\"");
// composites render as pretty JSON, because that is what the user will edit.
function renderValueForInput(value: unknown, type: RunnableArg["type"]): string {
  if (value === undefined || value === null) return "";
  if (type === "string" && typeof value === "string") return value;
  if (typeof value === "string" && (type === "any" || type === "number" || type === "boolean")) {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    // A value with a cycle cannot have come from JSON, so this is
    // unreachable through the normal path; returning empty is better than
    // throwing while painting a form.
    return "";
  }
}

export type CoerceResult =
  | { ok: true; values: Record<string, unknown> }
  | { ok: false; errors: Record<string, string> };

/**
 * coerceArgs turns the form's raw text back into JSON values.
 *
 * An EMPTY optional field is OMITTED from the result, not sent as null. The
 * two are different to the engine: an omitted argument lets the body's `??`
 * default apply and makes a `when(args.x)` guard drop its block, while an
 * explicit null is a supplied value that the guard treats as PRESENT. "I left
 * the box empty" means omitted.
 *
 * Every field is checked before returning, so the user sees all the problems
 * at once rather than one per attempt.
 */
export function coerceArgs(
  args: readonly RunnableArg[],
  raw: Readonly<Record<string, string>>,
): CoerceResult {
  const values: Record<string, unknown> = {};
  const errors: Record<string, string> = {};

  for (const arg of args) {
    const text = (raw[arg.name] ?? "").trim();

    if (text === "") {
      if (arg.required) {
        errors[arg.name] = "required";
      }
      continue;
    }

    if (arg.enum !== undefined && arg.enum.length > 0 && !arg.enum.includes(text)) {
      // The enum is the DSL's own closed set, so a value outside it is
      // guaranteed to be refused by the engine. Saying so here names the
      // field; the engine's refusal would not.
      errors[arg.name] = `must be one of: ${arg.enum.join(", ")}`;
      continue;
    }

    const coerced = coerceOne(arg.type, text);
    if ("error" in coerced) errors[arg.name] = coerced.error;
    else values[arg.name] = coerced.value;
  }

  if (Object.keys(errors).length > 0) return { ok: false, errors };
  return { ok: true, values };
}

function coerceOne(
  type: RunnableArg["type"],
  text: string,
): { value: unknown } | { error: string } {
  switch (type) {
    case "string":
      return { value: text };
    case "number": {
      const n = Number(text);
      if (!Number.isFinite(n)) return { error: `"${text}" is not a number` };
      return { value: n };
    }
    case "boolean": {
      const lowered = text.toLowerCase();
      if (lowered === "true") return { value: true };
      if (lowered === "false") return { value: false };
      return { error: `"${text}" is not true or false` };
    }
    case "object": {
      const parsed = parseJson(text);
      if ("error" in parsed) return { error: `not valid JSON: ${parsed.error}` };
      if (parsed.value === null || typeof parsed.value !== "object" || Array.isArray(parsed.value)) {
        return { error: "must be a JSON object" };
      }
      return { value: parsed.value };
    }
    case "array": {
      const parsed = parseJson(text);
      if ("error" in parsed) return { error: `not valid JSON: ${parsed.error}` };
      if (!Array.isArray(parsed.value)) return { error: "must be a JSON array" };
      return { value: parsed.value };
    }
    case "any": {
      // "any" is the degradation for a DSL type with no form equivalent, so
      // the box takes JSON -- and falls back to the literal text when the
      // input is not JSON. That fallback is what makes an unquoted word work
      // in a box the user has no reason to think of as a JSON editor.
      const parsed = parseJson(text);
      return { value: "error" in parsed ? text : parsed.value };
    }
    default:
      return { value: text };
  }
}

function parseJson(text: string): { value: unknown } | { error: string } {
  try {
    return { value: JSON.parse(text) as unknown };
  } catch (err) {
    return { error: err instanceof Error ? err.message : String(err) };
  }
}

/**
 * autoInjectedNotice is the honest label for a tool's argument form.
 *
 * A tool field can be marked `@autoInjected` in the DSL, which means the
 * engine supplies it server-side and DROPS whatever the caller sent. The
 * language server includes those fields in `args` with NO FLAG distinguishing
 * them (see cmd/memql-lsp/runnable.go), so the extension genuinely cannot tell
 * which ones they are -- and inventing a heuristic from field names would be
 * exactly the second parser the whole design forbids.
 *
 * So the form says so, once, rather than pretending. A developer who fills an
 * auto-injected field and sees their value ignored has an explanation on
 * screen instead of a mystery.
 */
export const AUTO_INJECTED_NOTICE =
  "Some tool fields are marked @autoInjected in the DSL: the engine supplies those itself and discards any value sent for them. The language server does not distinguish them from ordinary fields, so they appear below like any other -- a value you enter for one has no effect.";
