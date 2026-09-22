import type { Module, ModuleEnvVar } from "@znasllc-io/memql-sdk-core/client";

import type { ChipTone } from "../../../kit";
import { formatFreshness } from "../../../kit/format";
import type { Readiness } from "../../../live/readiness";
import { isModuleId } from "../../../system/modules";
import type { Verdict } from "../../../system/readinessFold";

// Reading the module inventory: everything about it that is a DECISION rather
// than a rendering, kept pure so it can be asserted without a DOM.

/**
 * The order the groups are drawn in, and it is NOT alphabetical.
 *
 * It runs from the most product-specific to the most structural, which is the
 * order an operator asks the questions in: a pack is what this INSTANCE was
 * configured to run, an integration is what it can talk to, a node-type is
 * which binaries the deployment brings up, and a component is the engine
 * itself. Sorted alphabetically it reads component, integration, node-type,
 * pack -- engine internals first and the one thing an operator can actually
 * change last.
 */
export const MODULE_KIND_ORDER = ["pack", "integration", "node-type", "component"] as const;

export type ModuleKindSlug = (typeof MODULE_KIND_ORDER)[number];

/** The heading each group carries. Plural, because a group is a set. */
export const MODULE_KIND_NAMES: Record<string, string> = {
  pack: "Packs",
  integration: "Integrations",
  "node-type": "Node types",
  component: "Components",
};

export interface ModuleGroup {
  kind: string;
  name: string;
  modules: Module[];
}

/**
 * The inventory, grouped by kind in the fixed order above.
 *
 * A kind the engine reports that this build has no name for is kept, in a
 * group of its own, AFTER the known ones -- dropping it would make a module
 * the cluster runs invisible on the page whose whole job is to list what the
 * cluster runs, and the failure would be silent. An empty group is not
 * rendered: a heading with nothing under it says the cluster has none of
 * something, which this read cannot actually promise (the answer is one
 * node's).
 */
export function groupModules(modules: readonly Module[]): ModuleGroup[] {
  const byKind = new Map<string, Module[]>();
  for (const module of modules) {
    const list = byKind.get(module.kind) ?? [];
    list.push(module);
    byKind.set(module.kind, list);
  }
  const groups: ModuleGroup[] = [];
  for (const kind of MODULE_KIND_ORDER) {
    const list = byKind.get(kind);
    if (list === undefined || list.length === 0) continue;
    groups.push({ kind, name: MODULE_KIND_NAMES[kind] ?? kind, modules: sortByName(list) });
    byKind.delete(kind);
  }
  for (const [kind, list] of [...byKind.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    if (list.length === 0) continue;
    groups.push({ kind, name: MODULE_KIND_NAMES[kind] ?? kind, modules: sortByName(list) });
  }
  return groups;
}

function sortByName(modules: Module[]): Module[] {
  return [...modules].sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * How a module's state reads.
 *
 * THREE BANDS, AND THE MIDDLE ONE IS THE POINT. "On" states are quiet -- a
 * component that is running is the ordinary case and a wall of accent chips
 * beside forty of them says nothing. "Off" states are muted, because a
 * compiled-out node type is a deliberate deployment choice rather than a
 * fault. What earns a warn tone is the band in between: a module that is
 * configured to run and CANNOT, because a credential is missing or somebody
 * opted it out. That is the only one an operator has anything to do about.
 *
 * An unrecognised state gets the neutral tone and its own word rendered
 * verbatim -- never mapped to the nearest thing this build happens to know,
 * which would be this surface guessing about a state the engine invented
 * after it was written.
 */
export function moduleStateTone(state: string): ChipTone {
  if (state === "credential_gated" || state === "opted_out") return "accent";
  if (
    state === "disabled" ||
    state === "compiled_out" ||
    state === "scaled_to_zero" ||
    state === "not_deployed"
  ) {
    return "muted";
  }
  return "neutral";
}

/** Whether a state is one an operator may need to act on. Drives the wording
 *  beside the chip, never a colour of its own. */
export function moduleStateNeedsAttention(state: string): boolean {
  return state === "credential_gated" || state === "opted_out";
}

/**
 * The sentence a state gets beside it, where the word alone is not enough.
 *
 * Only the two attention states carry one. Everything else is either obvious
 * (`running`, `disabled`) or has the engine's own `stateDetail` to say it,
 * and inventing a gloss for a state we did not write would put this file in
 * the business of explaining the engine to itself.
 */
export function moduleStateSentence(state: string): string {
  if (state === "credential_gated") return "configured to run, and missing a credential it needs";
  if (state === "opted_out") return "available, and switched off for this instance";
  return "";
}

/**
 * Whether this module's enablement is flippable at all.
 *
 * ONLY A PACK. `setPackEnabled` takes a pack domain and there is no sibling
 * call for anything else -- an integration's state is DERIVED from whether
 * its credentials resolve, and a node type's is its replica count. Rendering
 * a disabled switch for those would be a control that announces itself and
 * then refuses (DESIGN.md rule 12); rendering an enabled one would be a
 * control the engine has no method behind.
 */
export function isFlippable(module: Pick<Module, "kind">): boolean {
  return module.kind === "pack";
}

/**
 * The bar's detail line for a pack.
 *
 * THE RESTART SENTENCE IS NOT THE WHOLE ANSWER, and it used to be the only
 * one shown. A flippable module replaced the engine's own `stateDetail` with
 * "a flip is recorded now and read at next boot" -- true, and it meant an
 * operator about to enable a storefront pack saw when it would take effect
 * and never saw WHAT it would do. The engine says what a pack publishes to
 * the public (component/memql/module_registry.go); composing the two puts
 * the consequence on the screen where the decision is made.
 *
 * The engine's half goes FIRST. What this does matters more than when it
 * lands, and a reader who stops after one clause should have read the one
 * that could change their mind.
 */
export function packBarDetail(stateDetail: string): string {
  const restart =
    "A flip is recorded now and read by each node at its NEXT BOOT. Nothing running changes until they restart.";
  const engine = stateDetail.trim();
  return engine === "" ? restart : `${engine}. ${restart}`;
}

/**
 * Why a pack is off, when the engine has said which.
 *
 * TWO DIFFERENT FACTS WEAR ONE WORD. A pack that SHIPS disabled and has
 * never been flipped is waiting for somebody to enable it; a pack an
 * operator switched off is a decision with a reason behind it. Both render
 * as "Disabled", and reading the first as the second sends a person looking
 * for a row that does not exist.
 *
 * Read off the engine's own sentence rather than re-derived, because the
 * engine is the only side that can tell them apart -- it holds the declared
 * default and the row. An empty answer means the engine said nothing this
 * build recognises, and the caller renders nothing rather than guessing.
 */
export function packOffReading(stateDetail: string): string {
  if (stateDetail.includes("ships DISABLED")) {
    return "This pack ships disabled. No one has switched it off -- it has not been switched on.";
  }
  if (stateDetail.includes("set by an operator")) {
    return "An operator switched this off for this instance.";
  }
  return "";
}

/** Why a non-pack has no switch, in the terms of what DOES change it. */
export function noSwitchSentence(kind: string): string {
  if (kind === "integration") {
    return "An integration has no switch. Its state is derived from configuration -- it becomes usable when the credentials it names resolve, and stops when they do not.";
  }
  if (kind === "node-type") {
    return "A node type has no switch. Whether it runs is its replica count in the deployment, and whether it exists at all is the build tag its image was compiled with.";
  }
  if (kind === "component") {
    return "A component has no switch. It is part of the engine binary and is present wherever that binary runs.";
  }
  return "This kind of module has no switch here.";
}

/**
 * What an env var's value reads as.
 *
 * A SECRET IS `set` OR `unset` AND NEVER A VALUE. That is not this surface
 * being careful: the engine's contract is that `value` is always "" for a
 * secret entry, there is no reveal call, and the proto has no field one could
 * be added to. So the two words ARE the whole answer, and the function
 * refuses to fall through to `value` for a secret even if a future wire ever
 * carried one -- a UI that renders whatever it is handed is a UI that leaks
 * the day something upstream changes.
 */
export function envVarReading(v: Pick<ModuleEnvVar, "secret" | "set" | "value">): string {
  if (v.secret) return v.set ? "set" : "unset";
  if (!v.set) return "unset";
  return v.value === "" ? "set" : v.value;
}

/** The outcome sentence after a successful pack flip. `restartRequired` is
 *  ALWAYS true in v1, so the sentence is unconditional rather than branching
 *  on a field that has one value -- a branch nobody can reach reads as a
 *  promise that the other arm exists. */
export function flipOutcomeSentence(packDomain: string, enabled: boolean): string {
  return `${packDomain} is now recorded as ${enabled ? "enabled" : "disabled"}. Nothing running has changed: each node reads this at its NEXT BOOT, so it takes effect when the nodes restart.`;
}

/**
 * The cluster-wide readiness for a module row, when the registry's name is
 * also a readiness module id.
 *
 * TWO READINGS OF DIFFERENT SCOPE sit in one row, and that is the point of
 * putting them side by side. The inventory's `state` is THIS node's answer
 * about its own registries and environment; the verdict is every LIVE node's,
 * folded. They can honestly differ mid-rollout, which is why the column
 * carries the disagreement rather than a single word -- an operator reading
 * "configured" here while a sibling replica refuses every send has no way to
 * tell from one word which node they are looking at.
 *
 * Null when the registry module has no readiness counterpart, which is most
 * of them: the module registry is every compiled-in component, and readiness
 * declares the seven a person configures.
 */
export function readinessForModule(
  module: { name: string },
  readiness: Readiness | undefined,
): Verdict | null {
  if (!readiness || !readiness.loaded || !isModuleId(module.name)) return null;
  return readiness.of(module.name);
}

/**
 * One live node's line in a module's reading across the cluster.
 *
 * `counted` is the whole distinction this list exists to draw (memql#5259):
 * a node that voted, and a node the fold SET ASIDE -- behind the cluster, or
 * unable to check -- whose row is shown so an operator can see which node and
 * since when, and drawn quieter so it never reads as a vote.
 */
export interface ReadinessNodeLine {
  nodeId: string;
  nodeType: string;
  words: string;
  note: string;
  counted: boolean;
  /**
   * The node's own reportedAt, verbatim, for the row's title -- or "" when it
   * reported no time at all.
   *
   * WHY THE RAW STRING AND NOT A SECOND FORMATTED ONE. `note` carries the
   * freshness a person READS ("checked 40s ago"), which is the right answer to
   * "is this current" and the wrong one to "which of these two nodes read the
   * cluster first". Two nodes both "checked 2m ago" are ordered by nothing a
   * reader can see, and that ordering is exactly what the fold's staleness
   * rule turns on. Same pattern as Fleet's round trip, which puts `rttAt` on
   * the Fact's title beside the relative figure.
   */
  at: string;
}

/**
 * Every live node behind a verdict, voters first in the fold's own order
 * (worst state first), then the nodes catching up, then the ones that could
 * not check.
 *
 * The words are the node's OWN answer, in the vocabulary the rest of the
 * shell uses; the note says how fresh that answer is, or why it does not
 * count. A stale node's note names what it will do about it -- re-check on its
 * own -- because an operator reading "behind" without that is an operator who
 * goes and restarts a pod that needed nothing.
 */
export function readinessNodeLines(v: Verdict, now: Date): ReadinessNodeLine[] {
  const lines: ReadinessNodeLine[] = v.nodes.map((n) => ({
    nodeId: n.nodeId,
    nodeType: n.nodeType,
    words: nodeStateWords(n.state),
    note: `checked ${formatFreshness(n.reportedAt, now)}`,
    counted: true,
    at: n.reportedAt,
  }));
  for (const a of v.aside) {
    if (a.why === "stale") {
      lines.push({
        nodeId: a.nodeId,
        nodeType: a.nodeType,
        words: "Catching up",
        note: `last checked ${formatFreshness(a.reportedAt, now)}, before the last change; it re-checks on its own`,
        counted: false,
        at: a.reportedAt,
      });
      continue;
    }
    lines.push({
      nodeId: a.nodeId,
      nodeType: a.nodeType,
      words: "Could not check",
      note: `${unknownReasonWords(a.reason)}; it retries on its own`,
      counted: false,
      at: a.reportedAt,
    });
  }
  return lines;
}

/** A single node's state, in the Set up vocabulary. */
function nodeStateWords(state: string): string {
  if (state === "configured") return "Set up";
  if (state === "partial") return "Partly set up";
  if (state === "unconfigured") return "Not set up";
  // A state this build does not know is rendered verbatim rather than mapped
  // to the nearest thing -- the same refusal moduleStateTone makes.
  return state;
}

/**
 * The closed reason vocabulary (component/memql/readiness), in words. An
 * unrecognised reason says only that the check failed: the row never carries
 * an error string, so there is nothing more specific to render.
 */
export function unknownReasonWords(reason: string | undefined): string {
  if (reason === "fleetReadFailed") return "the fleet could not be read";
  if (reason === "integrationProbeFailed") return "the integration's status check failed";
  return "the check did not finish";
}
