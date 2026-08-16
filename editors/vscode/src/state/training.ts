// Per-construct training state, and what the editor makes of it.
//
// SAVING IS NOT PROMOTING. That is the single idea this whole surface exists to
// convey, and everything below is in service of it: a file may hold trained,
// untrained and drifted constructs at once, and writing it to disk changes none
// of them. Nothing here takes a "dirty" or "saved" input, so that property holds
// by construction rather than by a test somebody could delete.
//
// THE LANGUAGE SERVER OWNS THE STATE, exactly as it owns which constructs are
// runnable. Drift is "does this construct's source hash match what was
// promoted", the hash is defined once and pinned by a parity test (design §3),
// and a client re-deriving it would be a second opinion about drift -- the one
// question this surface exists to answer. So this file decodes and renders; it
// never computes a state.
//
// THE WIRE SHAPE IS #3759's, MIRRORED FROM `memql/runnableConstructs` field for
// field, because that issue says the state is exposed "the way
// memql/runnableConstructs already is". It is declared here before the server
// implements it (announced on #3761 / #3759 so ENGINE can correct it), which is
// safe in exactly one direction: an unrecognised reply degrades to `unknown`
// and `unknown` renders as nothing, so a wrong guess shows a developer no
// training UI rather than a wrong one.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3761 #3745

import type { LspRange } from "../constructs/runnable.js";

// -----------------------------------------------------------------------------
// The wire contract
// -----------------------------------------------------------------------------

/**
 * The six states, from design §2 plus memql#3928.
 *
 * `seeded` is distinct from `trained` and the distinction is the point: a
 * seeded construct was loaded from disk at boot, so the cluster HAS it but it
 * was never promoted -- and it cannot be, short of a rollout. Collapsing the
 * two would offer a Demote on something there is nothing to demote.
 *
 * `staged` is distinct from `trained` on the same kind of argument: it is
 * durable on the cluster and callable BY ITS AUTHOR ALONE, and it is the only
 * state with a Train action. Collapsing it into `trained` would claim the
 * cluster runs something only one person can call; collapsing it into
 * `untrained` would deny that the cluster has it at all, and offer a Promote
 * that is really a Train.
 */
export const TRAINING_STATES = [
  "untrained",
  "drifted",
  "trained",
  "seeded",
  "staged",
  "unknown",
] as const;
export type TrainingState = (typeof TRAINING_STATES)[number];

/**
 * The server capability key and method.
 *
 * Feature-detected on the capability rather than called blind, for the same
 * reason `memqlRunnableConstructs` is: an older `memql-lsp` on the PATH is an
 * ordinary situation, not an error worth a popup. Before #3759 lands, EVERY
 * server is that older build -- so this surface is simply absent until it does,
 * which is the correct behaviour and not a degraded one.
 */
export const TRAINING_STATE_CAPABILITY = "memqlTrainingState";
export const TRAINING_STATE_METHOD = "memql/trainingState";

export interface TrainingConstruct {
  /**
   * The catalog's spelling, carried through and never narrowed.
   *
   * Nothing in this surface dispatches on kind -- a drifted query and a drifted
   * tool get the same gutter and the same lens -- so narrowing it would be a
   * closed vocabulary this file has no use for and could only get wrong.
   */
  kind: string;
  name: string;
  /** Signature only: keyword through declared name. What a lens anchors to. */
  signatureRange: LspRange;
  state: TrainingState;
}

// -----------------------------------------------------------------------------
// Defensive decoding
// -----------------------------------------------------------------------------

/**
 * Narrow the raw JSON-RPC result.
 *
 * Same rules as `parseRunnableConstructs`, and for the same reason: the server
 * is a separate process, possibly an older build than this extension, so its
 * reply is untrusted input in the ordinary sense.
 *
 * ONE DIFFERENCE, and it is deliberate. An unrecognised `state` does not drop
 * the construct -- it becomes `unknown`, which renders as nothing. Dropping and
 * degrading look identical here (both show no training UI), so degrading is
 * chosen because it keeps the construct in the list for anything that counts
 * entries, and because "a state this build does not know" is genuinely closer
 * to "unknown" than to "absent".
 */
export function parseTrainingConstructs(raw: unknown): TrainingConstruct[] {
  if (raw === null || typeof raw !== "object") return [];
  const constructs = (raw as { constructs?: unknown }).constructs;
  if (!Array.isArray(constructs)) return [];
  const out: TrainingConstruct[] = [];
  for (const entry of constructs) {
    const parsed = parseOne(entry);
    if (parsed !== undefined) out.push(parsed);
  }
  return out;
}

function parseOne(raw: unknown): TrainingConstruct | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  const name = r.name;
  if (typeof name !== "string" || name === "") return undefined;
  const signatureRange = parseRange(r.signatureRange);
  if (signatureRange === undefined) return undefined;
  return {
    kind: typeof r.kind === "string" ? r.kind : "",
    name,
    signatureRange,
    state: isTrainingState(r.state) ? r.state : "unknown",
  };
}

function isTrainingState(value: unknown): value is TrainingState {
  return typeof value === "string" && (TRAINING_STATES as readonly string[]).includes(value);
}

function parseRange(raw: unknown): LspRange | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as { start?: unknown; end?: unknown };
  const start = parsePosition(r.start);
  const end = parsePosition(r.end);
  if (start === undefined || end === undefined) return undefined;
  return { start, end };
}

function parsePosition(raw: unknown): { line: number; character: number } | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const r = raw as { line?: unknown; character?: unknown };
  if (typeof r.line !== "number" || typeof r.character !== "number") return undefined;
  return { line: r.line, character: r.character };
}

// -----------------------------------------------------------------------------
// The gutter's three answers
// -----------------------------------------------------------------------------

/**
 * What the gutter distinguishes. THREE, not five.
 *
 * The gutter answers one question -- *does what I am looking at match what
 * runs?* -- and that question has three answers. `trained` and `seeded` are the
 * same answer to it (yes), and differ only in how the cluster came to have the
 * construct, which is a question about ACTIONS and belongs on the lens. Giving
 * them separate near-identical icons would spend the gutter's very limited
 * legibility on a distinction the gutter is not being asked about.
 *
 * `unknown` is absent from this type on purpose: it has no mark at all.
 */
export type GutterMark = "untrained" | "drifted" | "live";

/** The mark for a state, or undefined when the state gets no mark. */
export function gutterMarkFor(state: TrainingState): GutterMark | undefined {
  switch (state) {
    case "untrained":
      return "untrained";
    case "drifted":
      return "drifted";
    case "trained":
    case "seeded":
    // `staged` is LIVE, and no fourth mark. The gutter answers one question --
    // does what I am looking at match what runs? -- and for a staged construct
    // the answer is yes: it is on the cluster and it is this version of it. WHO
    // can call it is a question about actions, and the lens is where that is
    // said. A fourth glyph would spend the gutter's very limited legibility on
    // a distinction the gutter is not being asked about, which is the same
    // reasoning that keeps `trained` and `seeded` sharing this mark.
    case "staged":
      return "live";
    case "unknown":
      // NOTHING. Not a grey icon, not a question mark. A disconnection must not
      // look like a problem with the file -- and a mark that means "we do not
      // know" is indistinguishable at 16 pixels from one that means "something
      // is wrong here".
      return undefined;
  }
}

// -----------------------------------------------------------------------------
// The lens
// -----------------------------------------------------------------------------

export const COMMAND_DRY_RUN = "memql.training.dryRun";
export const COMMAND_TRY_IN_SESSION = "memql.training.tryInSession";
export const COMMAND_PROMOTE = "memql.training.promote";
export const COMMAND_DEMOTE = "memql.training.demote";
/**
 * Stage: make a construct durable for its AUTHOR ALONE (epic memql#3928).
 *
 * The fifth command, and the only one this surface adds for the staged tier.
 * TRAINING a staged construct reuses COMMAND_PROMOTE, because on the wire it IS
 * a promote -- the engine flips the same persisted row. A `memql.training.train`
 * command would have been a second name for one operation, and the lens says
 * which act it means through its title.
 */
export const COMMAND_STAGE = "memql.training.stage";

export interface TrainingAction {
  title: string;
  command: string;
}

export interface TrainingLensPlan {
  construct: TrainingConstruct;
  /** The leading label: the state, in the developer's words. */
  label: string;
  /** A sentence for the lens tooltip, or "" when the label says enough. */
  detail: string;
  actions: TrainingAction[];
}

export interface TrainingLensOptions {
  /**
   * Emit the action lenses. OFF by default, and not because the actions are
   * optional: they are #3763's, which registers the four commands. A lens that
   * posts to an unregistered command fails with "command not found", and a
   * click that does nothing teaches a developer the extension is broken.
   * #3763 turns this on in the same change that makes it work.
   *
   * The STATE label renders either way, because that half works standalone and
   * is the half this issue exists for.
   */
  offerActions?: boolean;
}

/**
 * The lens plan for each construct, in document order.
 *
 * An `unknown` construct produces NO PLAN -- not an empty one. The caller
 * renders what it gets, so absence here is absence on screen.
 */
export function trainingLensPlans(
  constructs: readonly TrainingConstruct[],
  options: TrainingLensOptions = {},
): TrainingLensPlan[] {
  const out: TrainingLensPlan[] = [];
  for (const construct of constructs) {
    if (construct.state === "unknown") continue;
    out.push({
      construct,
      label: construct.state,
      detail: detailFor(construct.state),
      actions: options.offerActions === true ? actionsFor(construct.state) : [],
    });
  }
  return out;
}

/** The sentence behind the one-word label, for the states where it is not obvious. */
function detailFor(state: TrainingState): string {
  switch (state) {
    case "untrained":
      return "This cluster has no record of this construct. Saving the file does not change that.";
    case "drifted":
      return "This cluster knows this construct, but not this version of it. Promoting updates the trained version.";
    case "trained":
      return "Promoted, persisted, and live on this cluster.";
    case "seeded":
      // The one state whose whole content is why there is nothing to do.
      return "Loaded from disk when the cluster booted, rather than promoted -- so there is nothing here to demote, and changing it needs a rollout.";
    case "staged":
      return "Staged on this cluster: persisted and replayed at boot, and callable by you and by nobody else. Train it to make it live for everyone.";
    case "unknown":
      return "";
  }
}

function actionsFor(state: TrainingState): TrainingAction[] {
  switch (state) {
    case "untrained":
      return [
        { title: "Dry-run", command: COMMAND_DRY_RUN },
        { title: "Try in session", command: COMMAND_TRY_IN_SESSION },
        // Stage sits BETWEEN the two it is offered with, and in that order,
        // because the order is the escalation: a session that ends, a private
        // one that does not, and one everybody gets.
        { title: "Stage", command: COMMAND_STAGE },
        { title: "Promote", command: COMMAND_PROMOTE },
      ];
    case "drifted":
      return [
        { title: "Dry-run", command: COMMAND_DRY_RUN },
        { title: "Try in session", command: COMMAND_TRY_IN_SESSION },
        { title: "Stage", command: COMMAND_STAGE },
        // Named, because promoting over an existing definition is a different
        // act from promoting a new one and the lens is where that is noticed.
        { title: "Promote (updates the trained version)", command: COMMAND_PROMOTE },
      ];
    case "trained":
      return [{ title: "Demote", command: COMMAND_DEMOTE }];
    case "staged":
      return [
        // Re-stage: how an edited staged construct is updated. Offered first
        // because it is the smallest of the three and the one a developer
        // iterating will reach for repeatedly.
        { title: "Re-stage", command: COMMAND_STAGE },
        // TRAIN IS COMMAND_PROMOTE, and the title is where the difference is
        // said. On the wire it IS a promote -- the engine flips the same
        // persisted row rather than writing a second one -- so a separate
        // command would have been a second name for one operation. The title
        // names the consequence, which is what the developer is deciding about.
        { title: "Train (make it live for everyone)", command: COMMAND_PROMOTE },
        { title: "Demote", command: COMMAND_DEMOTE },
      ];
    case "seeded":
    case "unknown":
      // NO ACTION, and rendered as the absence of one rather than as a disabled
      // control -- the same choice the Constructs page makes for a view-only
      // kind. A seeded construct needs a rollout, which is not something this
      // editor can offer.
      return [];
  }
}

// -----------------------------------------------------------------------------
// The status bar
// -----------------------------------------------------------------------------

/**
 * One counter per state except `unknown`.
 *
 * EVERY non-`unknown` state needs a key here, and the requirement is a runtime
 * one rather than a type one: countStates indexes this object BY STATE NAME, so
 * a state with no key increments `undefined` and writes NaN -- a status bar
 * reading "NaN untrained" rather than a compile error (memql#3938).
 */
export interface TrainingCounts {
  untrained: number;
  drifted: number;
  trained: number;
  seeded: number;
  staged: number;
}

export function countStates(constructs: readonly TrainingConstruct[]): TrainingCounts {
  const counts: TrainingCounts = { untrained: 0, drifted: 0, trained: 0, seeded: 0, staged: 0 };
  for (const c of constructs) {
    if (c.state !== "unknown") counts[c.state] += 1;
  }
  return counts;
}

/**
 * The status-bar text, or "" when there is nothing to say.
 *
 * REPORTS ONLY WHAT NEEDS ATTENTION -- untrained and drifted. A file whose
 * constructs are all trained gets an empty status bar, which is the correct
 * report: the item exists to make "I saved but did not promote" impossible to
 * miss, and an item that is always present saying "12 trained" is one a
 * developer stops reading, taking the warning with it.
 *
 * Zero counts are omitted rather than shown as `0 drifted`, for the same
 * reason.
 */
export function statusBarText(counts: TrainingCounts): string {
  const parts: string[] = [];
  if (counts.untrained > 0) parts.push(`${counts.untrained} untrained`);
  if (counts.drifted > 0) parts.push(`${counts.drifted} drifted`);
  return parts.join(" · ");
}

/** The hover, which is where "saving is not promoting" is said in words. */
export function statusBarTooltip(counts: TrainingCounts): string {
  if (statusBarText(counts) === "") return "";
  const lines = ["Saving this file does not promote anything to the cluster."];
  if (counts.untrained > 0) {
    lines.push(`${counts.untrained} construct(s) this cluster has no record of.`);
  }
  if (counts.drifted > 0) {
    lines.push(`${counts.drifted} construct(s) the cluster knows in an older version.`);
  }
  return lines.join("\n");
}

// -----------------------------------------------------------------------------
// The list the status bar clicks through to
// -----------------------------------------------------------------------------

/**
 * The command the status-bar item posts (design §4: "clicking through to a
 * list. This is what makes 'I saved but did not promote' impossible to miss").
 *
 * It NAVIGATES AND DOES NOT ACT -- it opens a picker and moves a cursor. That is
 * why it is the only training command safe to offer in the command palette,
 * where the four that submit something to a cluster are hidden.
 */
export const COMMAND_SHOW_LIST = "memql.training.showList";

/** One row of that list. */
export interface TrainingListEntry {
  construct: TrainingConstruct;
  /** The row's primary text: the construct, as the file declares it. */
  label: string;
  /** The row's secondary text: the state, in the same word the lens uses. */
  description: string;
  /** The row's third line: why that state matters. */
  detail: string;
}

/**
 * The constructs the status bar is counting, as a list.
 *
 * SAME SET, SAME MODULE, and that is the whole reason this lives here rather
 * than next to the picker that renders it. `statusBarText` reports untrained and
 * drifted and deliberately nothing else, so a list clicking through to any other
 * set would turn the number into a lie the moment somebody counted the rows. The
 * two derivations sit beside each other and read the same two states.
 *
 * DOCUMENT ORDER, not grouped by state. The list is a way back to a place in a
 * file; a developer scanning for the one they were just looking at knows roughly
 * where it sat, not which bucket it landed in.
 */
export function trainingListEntries(
  constructs: readonly TrainingConstruct[],
): TrainingListEntry[] {
  const out: TrainingListEntry[] = [];
  for (const construct of constructs) {
    if (construct.state !== "untrained" && construct.state !== "drifted") continue;
    out.push({
      construct,
      label: `${construct.kind} ${construct.name}`,
      description: construct.state,
      // The lens's own sentence, reused rather than rewritten: two wordings for
      // one state are two things to keep in step, and the second one drifts.
      detail: detailFor(construct.state),
    });
  }
  return out;
}
