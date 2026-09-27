import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import { overridden, stepFromRow, type Head, type RunRow, type StepOverrideRow, type StepRow } from "./rows";
import { axesPhrase, effortWord, levelWord } from "./words";

// The versions of a run's steps, and every decision the run page makes about
// them -- pure, for the reason rows.ts is: a projection asserted through
// render() is asserted through three layers that can each fail for unrelated
// reasons (epic memql#5414, design D18-D20).
//
// ===========================================================================
// TWO SOURCES, ONE ANSWER
// ===========================================================================
// The live steps feed carries ONE row per step: every version of a step is a
// version of the same row id, and the feed folds to the newest. The head
// re-assertion keeps that newest row equal to the CURRENT version, except
// while a re-run is writing a newer one. The versions read (`workStepVersions`)
// answers every version once, but it is a read and not a feed.
//
// So a step's versions are the read, with the feed's row laid over the one
// version it carries -- the feed is fresher for that version, and the read is
// the only thing that knows the others exist. Neither alone is enough: a head
// moved back to version 1 re-asserts version 1 as the newest row, and the
// feed then cannot tell there were ever three.

/** One version of one step, as the versions read answers it. */
export interface StepVersion extends StepRow {
  /** The read's own `current` mark, or null when it did not say. */
  current: boolean | null;
}

/**
 * One entry of the `workStepVersions` reply.
 *
 * THE WIRE ID IS `<stepId>@v<version>`, one distinct id per version because a
 * builtin reply is one id-keyed map (a shared id would collapse the versions
 * into one entry). The suffix is stripped here so a version names its step row
 * like the feed's row does; the version comes from the payload, falling back
 * to the suffix for an entry that did not repeat it.
 */
export function stepVersionFromRow(row: Row): StepVersion {
  const flat = flatten(row);
  const step = stepFromRow(flat);
  const match = /^(.*)@v(\d+)$/.exec(step.id);
  const suffix = match ? Number(match[2]) : null;
  const stated = typeof flat["version"] === "number" ? rowNumber(flat, "version") : null;
  const version = stated !== null && stated >= 1 ? Math.round(stated) : suffix !== null && suffix >= 1 ? suffix : step.version;
  return {
    ...step,
    id: match ? (match[1] as string) : step.id,
    version,
    current: typeof flat["current"] === "boolean" ? (flat["current"] as boolean) : null,
  };
}

/**
 * The read's versions, grouped by step key and ordered oldest first.
 *
 * ONE ENTRY PER VERSION. The read already folds each version to its newest
 * row-version, but a version that appears twice (a retry of the read racing a
 * write) keeps the one written last, so the intent row never shadows its own
 * receipt.
 */
export function versionsByKey(versions: readonly StepVersion[]): Map<string, StepVersion[]> {
  const byKey = new Map<string, Map<number, StepVersion>>();
  for (const version of versions) {
    if (version.key === "") continue;
    const held = byKey.get(version.key) ?? new Map<number, StepVersion>();
    const prior = held.get(version.version);
    if (prior === undefined || prior.createdAt <= version.createdAt) held.set(version.version, version);
    byKey.set(version.key, held);
  }
  const out = new Map<string, StepVersion[]>();
  for (const [key, held] of byKey) {
    out.set(
      key,
      [...held.values()].sort((a, b) => a.version - b.version),
    );
  }
  return out;
}

/** Every version of one step: the read, with the feed's row laid over the version it carries. */
export function versionsOf(
  read: ReadonlyMap<string, readonly StepVersion[]>,
  feed: StepRow | null,
  key: string,
): StepVersion[] {
  const byVersion = new Map<number, StepVersion>();
  for (const version of read.get(key) ?? []) byVersion.set(version.version, version);
  if (feed !== null && feed.key === key) {
    const prior = byVersion.get(feed.version);
    byVersion.set(feed.version, { ...feed, current: prior?.current ?? null });
  }
  return [...byVersion.values()].sort((a, b) => a.version - b.version);
}

/**
 * Which version of a step is current.
 *
 * THE LIVE HEAD FIRST. It is on the run row, which is a feed, so it is fresher
 * than the read's `current` mark -- which is itself only the head as the read
 * saw it. An entry that names ANOTHER run is a branch's reused prefix and
 * holds no version of this run, so it is skipped. Absent a head (a run written
 * before it existed) the concept's own rule applies: the newest row is
 * current.
 */
export function currentVersionOf(run: RunRow, versions: readonly StepVersion[], key: string): number | null {
  const entry = run.head[key];
  if (entry !== undefined && entry.runId === "") return entry.version;
  const marked = versions.find((version) => version.current === true);
  if (marked !== undefined) return marked.version;
  const newest = versions[versions.length - 1];
  return newest === undefined ? null : newest.version;
}

/** How many versions a step has, as far as anything on the page can tell. */
export function versionCount(versions: readonly StepVersion[], head: Head, feed: StepRow | null, key: string): number {
  let count = 0;
  for (const version of versions) count = Math.max(count, version.version);
  const entry = head[key];
  if (entry !== undefined && entry.runId === "") count = Math.max(count, entry.version);
  if (feed !== null) count = Math.max(count, feed.version, feed.attempt);
  return count;
}

/** The version a re-run would run as: one past the highest this page knows of. */
export function nextVersion(versions: readonly StepVersion[], head: Head, feed: StepRow | null, key: string): number {
  return versionCount(versions, head, feed, key) + 1;
}

/**
 * A re-run's rows signature: the key and version of every step the feed holds,
 * plus the head. The versions read is taken again when this changes -- a new
 * version appeared, or the head moved -- and NOT on a status flip, which the
 * feed already carries for the one version it holds.
 */
export function versionsSignature(run: RunRow, steps: readonly StepRow[]): string {
  const stepsPart = steps
    .map((step) => `${step.key}:${step.version}`)
    .sort()
    .join(",");
  const headPart = Object.keys(run.head)
    .sort()
    .map((key) => `${key}:${run.head[key]?.version ?? 0}`)
    .join(",");
  return `${stepsPart}|${headPart}`;
}

// ---------------------------------------------------------------------------
// What a version was asked for, and what answered it
// ---------------------------------------------------------------------------

/** "Reasoning, app:claude-code:opus, high effort" -- or "" when nothing was asked for. */
export function askedFor(o: StepOverrideRow): string {
  const parts: string[] = [];
  if (o.level !== "") parts.push(levelWord(o.level));
  if (o.model !== "") parts.push(o.model);
  if (o.effort !== "") parts.push(`${effortWord(o.effort).toLowerCase()} effort`);
  return parts.join(", ");
}

/**
 * Who answered, from the binding recorded at dispatch: "anthropic, claude-sonnet-4-5".
 *
 * ONLY WHAT WAS RECORDED. A binding with no model says nothing about who
 * answered, and the level a step asked for is not a model that served it.
 */
export function answeredBy(binding: Record<string, unknown> | null): string {
  if (binding === null) return "";
  const read = (key: string) => (typeof binding[key] === "string" ? (binding[key] as string).trim() : "");
  const provider = read("provider");
  const model = read("model");
  const effort = read("effort") || read("servedEffort");
  const parts: string[] = [];
  if (provider !== "") parts.push(provider);
  if (model !== "" && model !== provider) parts.push(model);
  if (effort !== "") parts.push(`${effortWord(effort).toLowerCase()} effort`);
  return parts.join(", ");
}

export interface OverrideFact {
  label: string;
  value: string;
  /** The whole value, when the line had to be cut. */
  title?: string;
  mono?: boolean;
}

/**
 * What the override changed beyond the level, model and effort (those are
 * "Asked for"), as facts: the words a person wrote, the inputs they changed,
 * and the dislike that rode along. The words themselves, not "instructions
 * were added" -- a version somebody authored should say what they said.
 */
export function overrideFacts(o: StepOverrideRow, session: boolean): OverrideFact[] {
  if (!overridden(o)) return [];
  const out: OverrideFact[] = [];
  if (o.prompt !== "") {
    out.push({ label: session ? "Prompt" : "Instructions", value: clip(o.prompt.trim()), title: o.prompt });
  }
  const inputs = Object.entries(o.inputs);
  if (inputs.length > 0) {
    const text = inputs.map(([key, value]) => `${key} = ${inputText(value)}`).join(", ");
    out.push({ label: inputs.length === 1 ? "Input" : "Inputs", value: clip(text), title: text, mono: true });
  }
  if (o.guidance !== null) {
    const phrase = axesPhrase(o.guidance);
    const reason = o.guidance.reason.trim();
    const what = phrase === "" ? "What was wrong" : `What was wrong with ${phrase}`;
    out.push({ label: "Passed on", value: reason === "" ? what : `${what} -- ${reason}` });
  }
  return out;
}

/**
 * The result, in one line: the error when there is one, else the value.
 *
 * TRUNCATED, NEVER REFORMATTED. A step's result is data and is set in the data
 * voice; a structured one is shown as compact JSON rather than paraphrased,
 * because a paraphrase is this window's guess about what matters in it.
 */
export function resultLine(result: Record<string, unknown> | null): string {
  if (result === null) return "";
  const error = result["error"];
  if (typeof error === "string" && error.trim() !== "") return clip(error.trim());
  const value = result["result"];
  if (typeof value === "string") return clip(value.trim());
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (value !== null && value !== undefined) {
    try {
      return clip(JSON.stringify(value));
    } catch {
      return "";
    }
  }
  const content = result["contentId"];
  if (typeof content === "string" && content !== "") return "Kept as a file";
  return "";
}

function clip(text: string): string {
  const firstLine = text.split("\n")[0] ?? "";
  return firstLine.length > 180 ? `${firstLine.slice(0, 179)}…` : firstLine;
}

// ---------------------------------------------------------------------------
// The consequence of an act, said before it is taken
// ---------------------------------------------------------------------------

/** How many top-level steps come after `key`. The run's own order wins; the timeline's is the fallback. */
export function stepsAfter(order: readonly string[], timeline: readonly string[], key: string): number {
  const source = order.includes(key) ? order : timeline;
  const index = source.indexOf(key);
  return index < 0 ? 0 : source.length - index - 1;
}

// EVERY ACT THAT RUNS A STEP AGAIN DOES ITS WORK AGAIN, outside the run
// included: a new version has a new idempotency key, and an app session run
// again is a new session. A message the step sent is sent again, and a command
// it ran on a machine runs again -- the one consequence a person deciding to
// re-run cannot see from the timeline, so it is said where the act is taken
// (SUPERVISED-VISUAL-COMPOSITION.md: never conceal material effects). Said as
// a conditional, because a step that only answered in words changed nothing.
export function rerunConsequence(version: number, after: number): string {
  const then =
    after === 0
      ? "Nothing after it runs again."
      : after === 1
        ? "The step after it runs again."
        : `The ${after} steps after it run again.`;
  const effects =
    after === 0 ? "Anything it changed outside the run is done again." : "Anything they changed outside the run is done again.";
  return `Runs as version ${version}. ${then} ${effects}`;
}

export const BRANCH_CONSEQUENCE =
  "Opens a new run from this step. The steps before it are reused, not run again; from this step on, anything changed outside the run is done again.";

// ---------------------------------------------------------------------------
// The composer's draft, and the ONLY-WHAT-CHANGED payload
// ---------------------------------------------------------------------------

export interface InputRow {
  id: string;
  key: string;
  value: string;
  /**
   * The value the version ran with, as text, or null for a row the person
   * added. A recorded row can be edited and never removed: leaving an input
   * out of a re-run keeps its value, so removing one would promise something
   * the call cannot do.
   */
  original: string | null;
}

export interface ComposerDraft {
  level: string;
  model: string;
  effort: string;
  /**
   * The prompt (a session step) or the instructions (any other step). NULL
   * until a session step's recorded prompt has been read, so a prompt nobody
   * has seen yet is never sent as if it had been edited.
   */
  text: string | null;
  inputs: InputRow[];
}

export interface ComposerBaseline {
  /** True when an app session answered the step: the text is then the WHOLE prompt. */
  session: boolean;
  /** The recorded session prompt; "" for an ordinary step, whose instructions start empty. */
  prompt: string;
}

/** A fresh draft: nothing changed, the recorded inputs laid out to edit. */
export function freshDraft(input: Record<string, unknown> | null, session: boolean): ComposerDraft {
  const inputs: InputRow[] = [];
  for (const [key, value] of Object.entries(input ?? {})) {
    const text = inputText(value);
    inputs.push({ id: `recorded:${key}`, key, value: text, original: text });
  }
  return { level: "", model: "", effort: "", text: session ? null : "", inputs };
}

/** How a recorded input value reads in its field: a string as itself, anything else as JSON. */
export function inputText(value: unknown): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value) ?? "";
  } catch {
    return String(value);
  }
}

/**
 * What a typed value means.
 *
 * JSON WHEN IT IS PLAINLY JSON, TEXT OTHERWISE: `42`, `true`, `[1, 2]`, `{"a":
 * 1}` and `"quoted"` are values, and anything else -- `2026-08`, `hello` -- is
 * the text it looks like. A leading character decides, so a word that happens
 * to parse is never turned into something else, and `null` stays the text
 * "null": a JSON null is dropped by the engine, which would make the row a
 * change that changes nothing.
 */
export function parseInputValue(text: string): unknown {
  const trimmed = text.trim();
  if (trimmed === "true") return true;
  if (trimmed === "false") return false;
  if (/^[[{"]/.test(trimmed) || /^-?\d/.test(trimmed)) {
    try {
      const parsed: unknown = JSON.parse(trimmed);
      return parsed === null ? text : parsed;
    } catch {
      return text;
    }
  }
  return text;
}

export interface ComposerArgs {
  level?: string;
  model?: string;
  effort?: string;
  prompt?: string;
  inputs?: Record<string, unknown>;
}

/**
 * The arguments to send: ONLY THE FIELDS THE PERSON CHANGED.
 *
 * Every field the builtin takes is optional and an absent one keeps the step's
 * own, so sending an unchanged value would not be harmless -- it would pin a
 * version to whatever the form happened to show, and mark the version as
 * authored by somebody who wrote nothing. A session prompt counts as changed
 * only when it differs from the recorded one; instructions only when there
 * are any; an input only when its value moved or the person added it.
 */
export function composerArgs(draft: ComposerDraft, baseline: ComposerBaseline): ComposerArgs {
  const args: ComposerArgs = {};
  if (draft.level !== "") args.level = draft.level;
  const model = draft.model.trim();
  if (model !== "") args.model = model;
  if (draft.effort !== "") args.effort = draft.effort;
  if (draft.text !== null) {
    if (baseline.session) {
      if (draft.text.trim() !== "" && draft.text !== baseline.prompt) args.prompt = draft.text;
    } else if (draft.text.trim() !== "") {
      args.prompt = draft.text.trim();
    }
  }
  const inputs: Record<string, unknown> = {};
  for (const row of draft.inputs) {
    const key = row.key.trim();
    if (key === "") continue;
    if (row.original !== null && row.value === row.original) continue;
    inputs[key] = parseInputValue(row.value);
  }
  if (Object.keys(inputs).length > 0) args.inputs = inputs;
  return args;
}

/** Whether anything in the draft would be sent. */
export function draftChanges(draft: ComposerDraft, baseline: ComposerBaseline): boolean {
  return Object.keys(composerArgs(draft, baseline)).length > 0;
}

// ---------------------------------------------------------------------------
// The reply of an act
// ---------------------------------------------------------------------------

/** The first reply row's string field, "" when absent. */
export function replyString(row: Row | null, key: string): string {
  return row === null ? "" : rowString(flatten(row), key);
}

/** The first reply row's number field, null when absent. */
export function replyNumber(row: Row | null, key: string): number | null {
  if (row === null) return null;
  const value = flatten(row)[key];
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}
