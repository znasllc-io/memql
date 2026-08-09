// The TypeScript half of the `memql/runnableConstructs` contract (memql#3309).
//
// THE LANGUAGE SERVER OWNS ALL .memql PARSING. Nothing in this file, or
// anywhere else in the extension, looks at construct syntax: the server hands
// over which constructs are runnable, what arguments each takes, and where its
// signature sits, and the extension renders that. Two parsers would mean the
// generated argument form can silently disagree with the compiler about what a
// construct accepts, and the developer finds out by running the wrong thing.
//
// The wire shape below is a FIXED contract mirrored from
// cmd/memql-lsp/runnable.go. Field names, the six-value `type` set, and the
// "empty array, never null" rule on `constructs` and `args` are all
// load-bearing; changing any of them means changing both sides in one commit.
//
// Deliberately free of `vscode` imports so it is unit-testable under bare
// `node --test`; constructs/lensProvider.ts is the adapter that turns these
// into editor lenses. The rule is enforced mechanically -- see
// cmd/memql-lsp/vscodeimportrule_test.go.

// -----------------------------------------------------------------------------
// The wire contract
// -----------------------------------------------------------------------------

// The five kinds the language server will ever return. spec / trait / prompt /
// seed / concept / shape / provider / builtin each need an execution semantic
// decided (which row does a spec evaluate against; who pays for a prompt's
// provider call) that the runtime-panel design explicitly defers.
export const RUNNABLE_KINDS = ["query", "mutate", "logic", "tool", "automation"] as const;
export type RunnableKind = (typeof RUNNABLE_KINDS)[number];

// The six form-level types. A DSL type with no form equivalent arrives as
// "any" rather than as an error, so an arg the editor cannot type still gets a
// JSON entry box instead of blocking the run.
export const RUNNABLE_ARG_TYPES = [
  "string",
  "number",
  "boolean",
  "object",
  "array",
  "any",
] as const;
export type RunnableArgType = (typeof RUNNABLE_ARG_TYPES)[number];

// LSP coordinates: 0-based lines, UTF-16 code-unit characters.
export interface LspPosition {
  line: number;
  character: number;
}

export interface LspRange {
  start: LspPosition;
  end: LspPosition;
}

export interface RunnableArg {
  name: string;
  type: RunnableArgType;
  required: boolean;
  // The closed value set from @enum("a", "b") or the first-class enum(...)
  // type. Absent when unconstrained.
  enum?: string[];
  // Already resolved by the server from whichever channel the construct kind
  // actually retains -- the `///` doc comment for query/mutate/logic args, the
  // field's @description(...) for tool fields. The extension just renders it.
  description?: string;
  // The field is marked @autoInjected: the engine stamps it server-side and
  // DROPS whatever the caller sent. Only a `tool` field can carry it, so it is
  // absent for every other kind (memql#3333).
  //
  // The field is still rendered and still sent. Dropping it client-side would
  // be an invisible divergence from what the engine actually does; marking it
  // tells the developer their value has no effect, which is the true statement.
  autoInjected?: boolean;
}

export interface RunnableTrigger {
  concept?: string;
  event?: string;
  schedule?: string;
}

export interface RunnableConstruct {
  kind: RunnableKind;
  name: string;
  // The signature-bound concept short name for `query <Concept> <name>`,
  // absent otherwise.
  concept?: string;
  // Spans the signature only -- keyword through declared name, no brace, no
  // body. This is what a CodeLens anchors to.
  signatureRange: LspRange;
  args: RunnableArg[];
  // The construct carries @disabled: the loader skips it, so it is not active
  // on any cluster right now and a run of it can only be refused (memql#3333).
  //
  // Per CLAUDE.md @disabled is a REVERSIBLE on/off switch, not deprecation, so
  // the construct still gets a lens -- one that says so, rather than offering a
  // run whose only possible outcome is a FAILED_PRECONDITION the developer then
  // cannot tell apart from a @filter miss.
  disabled?: boolean;
  // Automations only.
  trigger?: RunnableTrigger;
}

// The server capability key. Feature-detect on this rather than calling the
// request blind and handling MethodNotFound -- an older memql-lsp on the PATH
// is an ordinary situation, not an error worth a popup.
export const RUNNABLE_CONSTRUCTS_CAPABILITY = "memqlRunnableConstructs";
export const RUNNABLE_CONSTRUCTS_METHOD = "memql/runnableConstructs";

// -----------------------------------------------------------------------------
// Kind classification
// -----------------------------------------------------------------------------

// Which kinds get the GENERATED ARGUMENT FORM (state/argForm.ts), built from
// the construct's own declared `args` block.
//
// An automation is the exception, and it is an exception about the DSL rather
// than about this extension: an automation binds its whole triggering event as
// `args` and reads `args.payload.<field>` freely, so there is no declared
// payload schema and `memql/runnableConstructs` returns `args: []` for every
// one of them. Generating a form from that would produce an empty form. Its
// run surface is state/automationForm.ts (pick a row of the trigger concept,
// or paste JSON) instead -- see memql#3310.
export const ARG_FORM_RUNNABLE_KINDS: readonly RunnableKind[] = ["query", "mutate", "logic", "tool"];

export function usesArgForm(kind: RunnableKind): boolean {
  return ARG_FORM_RUNNABLE_KINDS.includes(kind);
}

// Session-define covers the plain construct family. A `tool` is a declaration
// bound to a GO-BACKED HANDLER and an `automation` is event-triggered, so
// neither can be injected from a buffer -- running one runs the DEPLOYED
// definition. The UI has to say so; see webview/runPanel.ts's banner.
export function isSessionDefinable(kind: RunnableKind): boolean {
  return kind === "query" || kind === "mutate" || kind === "logic";
}

// Write kinds take the non-local-cluster confirmation. Reads run freely.
//
// `automation` is here because an automation run is the LARGEST write this
// extension can issue: it executes the automation's whole action chain --
// writes, LLM calls, and any downstream automations those writes trigger --
// so the "am I pointed at the right cluster" friction that a mutation earns,
// an automation earns several times over.
//
// `logic` is deliberately NOT here even though a logic body can call a
// mutation: the confirmation is friction against running against the wrong
// window, not a permission system (the engine's per-row authorization remains
// the only authority), and prompting on every logic run would train the
// developer to dismiss the dialog without reading it -- which is exactly how
// the mutation prompt stops working. `tool` is likewise excluded: it is
// declared, not authored here, and the deployed handler's own authorization
// applies.
export function isWriteKind(kind: RunnableKind): boolean {
  return kind === "mutate" || kind === "automation";
}

// -----------------------------------------------------------------------------
// Defensive decoding
// -----------------------------------------------------------------------------

// parseRunnableConstructs narrows the raw JSON-RPC result.
//
// The language server is a separate process, possibly an OLDER BUILD than this
// extension (memql.lsp.serverPath points at a user-chosen binary, and a
// PATH-resolved one is whatever is installed). So the reply is untrusted input
// in the ordinary sense: a field that moved, a kind this build does not know,
// or a null where the contract promises an array. Every one of those degrades
// to "this construct is not runnable" rather than throwing -- a thrown error
// inside provideCodeLenses surfaces to the user as a popup on a document they
// are simply still typing.
export function parseRunnableConstructs(raw: unknown): RunnableConstruct[] {
  if (raw === null || typeof raw !== "object") return [];
  const constructs = (raw as { constructs?: unknown }).constructs;
  if (!Array.isArray(constructs)) return [];
  const out: RunnableConstruct[] = [];
  for (const entry of constructs) {
    const parsed = parseOne(entry);
    if (parsed !== undefined) out.push(parsed);
  }
  return out;
}

function parseOne(raw: unknown): RunnableConstruct | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;

  const kind = r.kind;
  if (typeof kind !== "string" || !isRunnableKind(kind)) return undefined;
  const name = r.name;
  if (typeof name !== "string" || name === "") return undefined;
  const signatureRange = parseRange(r.signatureRange);
  if (signatureRange === undefined) return undefined;

  const out: RunnableConstruct = {
    kind,
    name,
    signatureRange,
    // The contract promises an array, but a construct with a MISSING args
    // array and one with an EMPTY one mean the same thing to the form
    // generator, so a missing field degrades rather than dropping the lens.
    args: Array.isArray(r.args) ? r.args.map(parseArg).filter(isDefined) : [],
  };
  if (typeof r.concept === "string" && r.concept !== "") out.concept = r.concept;
  // The server omits `disabled` for an enabled construct (`omitempty`), and an
  // older server never sends it at all. Both read as "not known to be
  // disabled", which is the safe default: the run is offered as before and the
  // refusal path still explains itself, exactly as it did before #3333.
  if (r.disabled === true) out.disabled = true;
  const trigger = parseTrigger(r.trigger);
  if (trigger !== undefined) out.trigger = trigger;
  return out;
}

function parseArg(raw: unknown): RunnableArg | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  const name = r.name;
  if (typeof name !== "string" || name === "") return undefined;
  // An unknown type degrades to "any" -- a JSON entry box -- rather than
  // dropping the arg. Dropping it would generate a form the compiler does not
  // agree with, which is the exact failure the single-parser rule exists to
  // prevent; a widened input box merely asks the developer to type the value.
  const type = typeof r.type === "string" && isRunnableArgType(r.type) ? r.type : "any";
  const out: RunnableArg = { name, type, required: r.required === true };
  if (Array.isArray(r.enum)) {
    const values = r.enum.filter((v): v is string => typeof v === "string");
    if (values.length > 0) out.enum = values;
  }
  if (typeof r.description === "string" && r.description !== "") {
    out.description = r.description;
  }
  // Omitted by the server for an ordinary field, and never sent at all by a
  // server older than #3333. Both mean "not known to be auto-injected", which
  // renders the field as an ordinary input -- the pre-#3333 behaviour.
  if (r.autoInjected === true) out.autoInjected = true;
  return out;
}

function parseTrigger(raw: unknown): RunnableTrigger | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  const out: RunnableTrigger = {};
  if (typeof r.concept === "string" && r.concept !== "") out.concept = r.concept;
  if (typeof r.event === "string" && r.event !== "") out.event = r.event;
  if (typeof r.schedule === "string" && r.schedule !== "") out.schedule = r.schedule;
  return Object.keys(out).length > 0 ? out : undefined;
}

function parseRange(raw: unknown): LspRange | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  const start = parsePosition(r.start);
  const end = parsePosition(r.end);
  if (start === undefined || end === undefined) return undefined;
  return { start, end };
}

function parsePosition(raw: unknown): LspPosition | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  // protojson omits a zero int32, and glsp's JSON does not -- but a
  // hand-rolled or older server might. A range anchored at the document start
  // legitimately carries 0/0, so an absent coordinate reads as 0 rather than
  // disqualifying the construct.
  const line = numberOr(r.line, 0);
  const character = numberOr(r.character, 0);
  if (line === undefined || character === undefined) return undefined;
  return { line, character };
}

function numberOr(v: unknown, fallback: number): number | undefined {
  if (v === undefined || v === null) return fallback;
  if (typeof v === "number" && Number.isFinite(v) && v >= 0) return Math.floor(v);
  return undefined;
}

function isDefined<T>(v: T | undefined): v is T {
  return v !== undefined;
}

function isRunnableKind(v: string): v is RunnableKind {
  return (RUNNABLE_KINDS as readonly string[]).includes(v);
}

function isRunnableArgType(v: string): v is RunnableArgType {
  return (RUNNABLE_ARG_TYPES as readonly string[]).includes(v);
}

// -----------------------------------------------------------------------------
// Lens planning
// -----------------------------------------------------------------------------

// RunTarget is what a lens command carries: everything the run orchestrator
// needs, with no live object references, so it survives the command-argument
// round trip VS Code performs.
export interface RunTarget {
  uri: string;
  kind: RunnableKind;
  name: string;
  args: RunnableArg[];
}

/**
 * AutomationTarget is what the automation lens command carries.
 *
 * Deliberately NOT a RunTarget. An automation's `args` is always empty (there
 * is no declared payload schema) and its `trigger` is the field that decides
 * the entire form -- which payload modes are offered, and which concept the
 * row picker browses. Carrying the B2 shape would mean an always-empty field
 * plus a lost one. Like RunTarget it holds no live object references, so it
 * survives the command-argument round trip VS Code performs.
 */
export interface AutomationTarget {
  uri: string;
  name: string;
  trigger?: RunnableTrigger;
  /**
   * The automation is @disabled in the buffer.
   *
   * Carried on the target because the REFUSAL is where it pays off: the engine
   * answers both "@disabled" and "@filter rejected this event" with
   * FAILED_PRECONDITION, and nothing in the reply distinguishes them. Knowing
   * the construct was disabled before the click is what lets the refusal say
   * which one happened instead of naming both (memql#3333).
   */
  disabled?: boolean;
}

export interface LensPlan {
  range: LspRange;
  title: string;
  tooltip: string;
  command: string;
  // Exactly one of these is present, chosen by `command`: the arg-form kinds
  // carry a RunTarget, an automation carries an AutomationTarget.
  target?: RunTarget;
  automationTarget?: AutomationTarget;
}

export const COMMAND_RUN = "memql.run.construct";
export const COMMAND_RUN_WITH = "memql.run.constructWith";
export const COMMAND_RUN_AUTOMATION = "memql.run.automation";

// lensPlansFor turns the server's answer into the lens set for one document.
//
// NOTHING HERE EXECUTES ANYTHING. A CodeLens renders an affordance; the
// command fires only on a click. There is no run-on-open and no run-on-save
// anywhere in the extension -- run configurations live in the workspace, so a
// repository can ship one, and a repository must never be able to make the
// editor talk to a cluster by being opened.
export function lensPlansFor(
  uri: string,
  constructs: readonly RunnableConstruct[],
): LensPlan[] {
  const out: LensPlan[] = [];
  for (const c of constructs) {
    if (!usesArgForm(c.kind)) {
      // An automation gets ONE lens, and it opens the form rather than
      // running anything: there is no declared payload schema, so there is no
      // "just run it" that would not be the extension inventing an event.
      // Even the schedule case -- which genuinely fires with an empty event --
      // goes through the form, because that form is where the developer is
      // told the DEPLOYED definition is what will run.
      const automationTarget: AutomationTarget = { uri, name: c.name };
      if (c.trigger !== undefined) automationTarget.trigger = c.trigger;
      if (c.disabled === true) automationTarget.disabled = true;
      out.push({
        range: c.signatureRange,
        title: c.disabled === true ? "Run automation (@disabled)..." : "Run automation...",
        tooltip: automationTooltip(c),
        command: COMMAND_RUN_AUTOMATION,
        automationTarget,
      });
      continue;
    }
    const target: RunTarget = { uri, kind: c.kind, name: c.name, args: c.args };
    out.push({
      range: c.signatureRange,
      title: c.disabled === true ? "Run (@disabled)" : "Run",
      tooltip: runTooltip(c),
      command: COMMAND_RUN,
      target,
    });
    out.push({
      range: c.signatureRange,
      title: "Run with...",
      tooltip: "Open the argument form for this construct.",
      command: COMMAND_RUN_WITH,
      target,
    });
  }
  return out;
}

// disabledPrefix is the one sentence a @disabled construct's tooltip leads
// with, on every runnable kind.
//
// It leads rather than trails because it changes what the rest of the tooltip
// MEANS: "run this against the selected cluster" is not true of a construct the
// loader skipped. The lens is kept (rather than dropped) because @disabled is a
// reversible switch and the developer looking at the declaration is usually the
// person about to re-enable it -- a construct that silently loses its
// affordance reads as a broken extension, not as a disabled construct.
//
// Session-definable kinds get the second sentence: a query / mutate / logic is
// injected from the buffer, so the run genuinely can succeed once the
// annotation is gone -- and, since the injected definition is what runs,
// removing the annotation in the buffer is enough. A tool or automation runs
// the DEPLOYED definition, so only a redeploy helps.
function disabledPrefix(kind: RunnableKind): string {
  const remedy = isSessionDefinable(kind)
    ? "Remove the annotation in this buffer and the run will use the enabled definition."
    : "It runs the DEPLOYED definition, so it stays refused until the annotation is removed and redeployed.";
  return `This ${kind} is @disabled: the loader skips it, so a run can only be refused. ${remedy}`;
}

function runTooltip(c: RunnableConstruct): string {
  if (c.disabled === true) return disabledPrefix(c.kind);
  const required = c.args.filter((a) => a.required).length;
  if (c.kind === "tool") {
    // Said on the lens as well as in the result banner: by the time the
    // banner is on screen the developer has already read the result.
    return "Run the DEPLOYED tool. A tool is bound to a Go-backed handler and cannot be defined from this buffer.";
  }
  if (required > 0) {
    return `Run this ${c.kind} against the selected cluster. ${required} required argument${required === 1 ? "" : "s"} -- the form opens when any is unset.`;
  }
  return `Run this ${c.kind} against the selected cluster, from this buffer, without saving.`;
}

// The automation tooltip says the deployed definition runs BEFORE the click,
// as the tool one does -- by the time the result banner is on screen the
// developer has already read the trace.
function automationTooltip(c: RunnableConstruct): string {
  if (c.disabled === true) return disabledPrefix(c.kind);
  const where =
    c.trigger?.schedule !== undefined && c.trigger.schedule !== "" && (c.trigger.event ?? "") === ""
      ? "It is time-driven, so the run fires it now with an empty event."
      : c.trigger?.concept !== undefined && c.trigger.concept !== ""
        ? `Build the trigger event from a row of ${c.trigger.concept}, or paste one.`
        : "Build the trigger event by pasting its payload.";
  return `Run the DEPLOYED automation with a synthetic trigger event. An automation cannot be session-defined, so this never runs your buffer. ${where}`;
}
