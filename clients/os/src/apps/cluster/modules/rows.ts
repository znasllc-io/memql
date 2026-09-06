import type { Module, ModuleEnvVar } from "@znasllc-io/memql-sdk-core/client";

import type { ChipTone } from "../../../kit";

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
