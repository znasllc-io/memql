// The automation form's model: how a synthetic trigger event gets built when
// there is no declared schema to generate fields from.
//
// EVERY OTHER RUNNABLE KIND HAS AN `args` BLOCK. The language server reports
// it, state/argForm.ts turns it into typed fields, and the developer fills
// them in. An automation has none: it binds its whole triggering event as
// `args` and reads `args.payload.<field>` freely, so `memql/runnableConstructs`
// returns `args: []` for every automation and there is nothing to generate
// from. That absence is not a gap in the LSP contract -- it is the truth about
// the construct, and it is the entire reason this module exists rather than
// automations reusing argForm.ts.
//
// So the payload is built one of three ways, chosen by what the automation's
// own `@trigger` says:
//
//  1. SCHEDULE. `@trigger(schedule="0 */10 * * * *")` has no concept and no
//     event. A cron firing hands the executor an EMPTY event, so the form
//     collapses to a single confirm -- no payload box, and emphatically no row
//     picker over a concept that does not exist.
//  2. ROW. `@trigger(event="graph.node.created", concept="v1:cognition:participant")`
//     names a concept, so the honest test payload is a REAL ROW of it. The
//     picker is the Concepts browser B1 already built (same paging call, same
//     state machine, same renderer); this module only owns the projection from
//     a picked row to the event payload.
//  3. JSON. A payload with no corresponding stored row, typed by hand. It is
//     validated as JSON HERE, before the request is sent, so a typo reads as a
//     form error next to the box rather than as a failed run against the
//     cluster.
//
// Row mode and JSON mode share ONE payload text box on purpose. Picking a row
// fills it with that row's JSON, which the developer can then edit -- so
// "pick a row and tweak one field" is reachable, and the text on screen is
// always exactly what will be sent. Two disconnected inputs would have made
// the sent payload something the form only implied.
//
// Deliberately free of `vscode` imports; webview/automationPanel.ts renders
// these. Tested under bare `node --test`.

import type { RunnableTrigger } from "../constructs/runnable.js";

/** How the event payload is built. */
export type AutomationFormMode = "schedule" | "row" | "json";

export interface AutomationFormPlan {
  /** The modes this automation admits, in the order the form offers them. */
  modes: AutomationFormMode[];
  /** The mode the form opens on. */
  defaultMode: AutomationFormMode;
  /**
   * The concept whose rows the picker browses. Empty when the trigger names
   * none -- the row picker is then not offered at all rather than shown
   * pointing at nothing.
   */
  conceptId: string;
  /** "schedule" | "event" | "manual" -- what the engine will call this run. */
  triggerKind: string;
  /** One sentence explaining what this automation's run will fire. */
  explanation: string;
}

/**
 * automationFormPlan decides which payload modes an automation admits.
 *
 * The decision is driven ENTIRELY by the trigger the language server reported,
 * never by a fallback that offers every mode and lets the engine refuse. A
 * scheduled automation shown an empty row picker would be a form asking a
 * question with no answer; a glob-triggered one shown a picker over a concept
 * it does not name would send a payload the engine cannot make a topic from.
 */
export function automationFormPlan(
  name: string,
  trigger: RunnableTrigger | undefined,
): AutomationFormPlan {
  const schedule = trigger?.schedule ?? "";
  const event = trigger?.event ?? "";
  const concept = trigger?.concept ?? "";

  // Schedule first: a time-driven automation has no event to synthesize, and
  // the engine fires it with an empty event. `@trigger` carries either a
  // schedule or an event, so an automation declaring both is malformed --
  // treating it as scheduled matches the engine, which asks IsScheduled()
  // only after IsEventTriggered() has said no.
  if (schedule !== "" && event === "") {
    return {
      modes: ["schedule"],
      defaultMode: "schedule",
      conceptId: "",
      triggerKind: "schedule",
      explanation: `${name} is time-driven (@trigger(schedule="${schedule}")). Running it fires it NOW with an empty event, exactly as a cron firing would.`,
    };
  }

  if (event !== "") {
    if (concept !== "") {
      return {
        modes: ["row", "json"],
        defaultMode: "row",
        conceptId: concept,
        triggerKind: "event",
        explanation: `${name} triggers on ${event} for ${concept}. Pick a real row of that concept, or paste a payload for a row that does not exist.`,
      };
    }
    return {
      modes: ["json"],
      defaultMode: "json",
      conceptId: "",
      triggerKind: "event",
      explanation: `${name} triggers on ${event}, which names no concept, so there is no row set to pick from. Paste the payload the event would have carried.`,
    };
  }

  // No trigger the server could report. The engine treats such a run as
  // "manual": an empty payload fires with an empty event, a supplied one
  // rides a synthetic automation.invocation.<name> topic.
  return {
    modes: ["json"],
    defaultMode: "json",
    conceptId: "",
    triggerKind: "manual",
    explanation: `${name} declares no trigger this build can read. The run is dispatched manually -- leave the payload empty to fire with an empty event.`,
  };
}

export type PayloadParse =
  | { ok: true; payload: Record<string, unknown> | undefined }
  | { ok: false; error: string };

/**
 * parsePayloadText validates the payload box BEFORE anything is sent.
 *
 * Three outcomes, and the middle one is the point of the function:
 *
 *  - Empty text is `payload: undefined`, which the client OMITS from the
 *    request. That is not the same as `{}`: the engine's buildRunEvent
 *    branches on `len(payload) == 0` to fire with an empty event, and an
 *    empty object takes the same branch -- but omitting is what the SDK
 *    documents for "no payload", so it is what gets sent.
 *  - Text that is not JSON, or is JSON but not an OBJECT, is a FORM error.
 *    The wire field is a struct, so an array or a bare scalar cannot be
 *    carried at all; reporting that here names the box, while letting it
 *    through produces an engine-side refusal naming the automation.
 *  - Anything else is the payload.
 */
export function parsePayloadText(text: string): PayloadParse {
  const trimmed = text.trim();
  if (trimmed === "") return { ok: true, payload: undefined };

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch (err) {
    return { ok: false, error: `not valid JSON: ${err instanceof Error ? err.message : String(err)}` };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return {
      ok: false,
      error: "the trigger event's payload must be a JSON object -- an array or a bare value cannot be carried as an event payload",
    };
  }
  return { ok: true, payload: parsed as Record<string, unknown> };
}

/**
 * payloadTextForRow renders a picked row as the payload text.
 *
 * The row goes in WHOLE -- intrinsics (`id`, `concept`, `createdAt`) alongside
 * the nested `payload` object -- because that is the shape a real graph event
 * carries, and an automation body reads `args.payload.id` and
 * `args.payload.payload.<field>` accordingly. Flattening it (as the row LIST
 * does, for display) would produce a payload no real firing ever delivers, and
 * an automation tested against it would pass here and fail in production.
 */
export function payloadTextForRow(row: Readonly<Record<string, unknown>>): string {
  try {
    return JSON.stringify(row, null, 2);
  } catch {
    // Unreachable from wire data (protojson cannot produce a cycle), but the
    // picker must never be the thing that takes the form down.
    return "";
  }
}

/**
 * automationConfirmationMessage is the non-local write confirmation's text.
 *
 * It differs from the mutation one in what it warns about, and the difference
 * is the whole reason automations count as a write kind: a mutation writes the
 * rows it declares, while an automation runs its ENTIRE action chain -- writes,
 * LLM calls, and whatever downstream automations those writes trigger. A
 * developer who has internalised "the mutation prompt means one row" needs to
 * be told this one means more.
 */
export function automationConfirmationMessage(name: string, clusterLabel: string): string {
  return `Run the automation "${name}" against ${clusterLabel}? That cluster is not marked local in clusters.yaml. An automation run executes its whole action chain -- writes, LLM calls, and any downstream automations those writes trigger -- against real data.`;
}

/**
 * DEPLOYED_DEFINITION_FALLBACK stands in when the engine's own
 * `accepted.definitionNote` is empty.
 *
 * The engine always sends one, and the UI renders THAT rather than this: the
 * note is ready-to-render text owned by the side that knows what actually ran.
 * But "the deployed definition ran, not your buffer" is an acceptance
 * criterion, not a nicety, so it must not disappear because a frame arrived
 * without its text.
 */
export const DEPLOYED_DEFINITION_FALLBACK =
  "Automations are not session-definable: the DEPLOYED definition on the cluster ran, not your editor buffer. Redeploy to run your edits.";

/** definitionBanner picks the engine's note, falling back to the constant above. */
export function definitionBanner(accepted: { ranDeployedDefinition: boolean; definitionNote: string }): string {
  // Gated on the flag as the issue requires. The engine sets it true on every
  // run today; rendering the banner unconditionally would make the flag
  // decorative, and a future run that genuinely DID execute a buffer would
  // then carry a banner saying the opposite.
  if (!accepted.ranDeployedDefinition) return "";
  return accepted.definitionNote.trim() === "" ? DEPLOYED_DEFINITION_FALLBACK : accepted.definitionNote;
}

/**
 * TARGET_NODE_TYPE_NOTICE explains the one field on the form that is not about
 * the payload.
 *
 * An automation's steps can reach integrations compiled into a single node
 * type, so "run it where those integrations live" is a real request rather
 * than an edge case -- and getting it wrong is the difference between a run
 * that works and an UNAVAILABLE refusal that reads like an outage.
 */
export const TARGET_NODE_TYPE_NOTICE =
  "Leave blank to run on the node that receives the request. Name a node type (cognition, agent, planner, ...) when the automation's steps reach integrations compiled into that node type only -- the run then travels the mesh and the trace comes back the same way.";
