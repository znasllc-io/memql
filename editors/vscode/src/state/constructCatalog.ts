// What the cluster has loaded, grouped for the Constructs tree.
//
// `ListConstructs` reads the LIVE registries -- the function registry, the
// specs, the concept registry, the tool/prompt/provider registries -- so a
// promoted construct appears the moment it is promoted and a demoted one
// disappears. That is what makes this a different question from the pack
// browser's: the browser answers "show me this file", and this answers "what do
// you have".
//
// THIS MODULE IS THE SEAM, and it has exactly two jobs beyond grouping. Both
// are closed vocabularies the ENGINE owns and delivers as a bare string, which
// the extension's existing run path types more narrowly:
//
//  1. THE KIND. The catalog reports `mutation`; the run path's `RunnableKind`
//     spells it `mutate`, because that is the keyword it is authored under.
//     The engine keeps the same mapping internally (`constructKeyword` in
//     component/memql/construct_catalog.go) for slicing source, but does not
//     put it on the wire -- so it is made here, once, rather than at each of
//     the several places that ask "can I run this".
//  2. THE ARG TYPE. `ConstructArg.type` arrives as `string`. `RunnableArg.type`
//     is the six-value `RunnableArgType` the argument form binds. An
//     unrecognised value narrows to `any`, which is not an invention: it is the
//     rule `constructs/runnable.ts` already states for the LSP path -- "a DSL
//     type with no form equivalent arrives as `any` rather than as an error, so
//     an arg the editor cannot type still gets a JSON entry box instead of
//     blocking the run". Defaulting to `string` instead would render a text box
//     for a value the engine will refuse, which is a confidently wrong degrade
//     rather than a cautious one.
//
// Both were raised on memql#3750 as candidates for the SDK. Neither was taken
// there, and the narrowing has to exist for the argument form to bind at all --
// so it lives here, in one function, where a reviewer can see both mappings
// side by side. If the SDK later types them, this collapses to a pass-through.
//
// NOTE ON #3753's WARNING: it says that if `argForm.ts`, `runResult.ts` or
// `src/run/*` need editing, the shapes have diverged and the fix belongs in the
// engine's parity gate. None of them is touched. The narrowing is at the
// boundary, in this file, on the way IN -- which is the difference between
// adapting a wire type and changing what the run path accepts.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3752 #3747

import type { Construct, ConstructArg } from "@znasllc-io/memql-sdk-core/constructs";

import {
  RUNNABLE_ARG_TYPES,
  type RunnableArg,
  type RunnableArgType,
  type RunnableKind,
  type RunnableTrigger,
} from "../constructs/runnable.js";

/**
 * The kind vocabulary, in the order the tree draws it.
 *
 * Runnable kinds first, because they are what an operator opens this view to
 * do something with; the view-only kinds follow in the order the DSL's own
 * dependency tree introduces them. A kind the engine reports that is NOT in
 * this list still renders -- see `groupByKind` -- because a client that hid a
 * construct because it had not heard of its kind would be silently
 * under-reporting what the cluster has loaded, which is the one thing this
 * view exists to be trusted about.
 */
export const KIND_ORDER: readonly string[] = [
  "query",
  "mutation",
  "logic",
  "tool",
  "automation",
  "concept",
  "shape",
  "spec",
  "trait",
  "prompt",
  "provider",
  "builtin",
  "policy",
  "seed",
];

/** The plural label a kind group carries. */
export const KIND_LABELS: Readonly<Record<string, string>> = {
  query: "queries",
  mutation: "mutations",
  logic: "logic",
  tool: "tools",
  automation: "automations",
  concept: "concepts",
  shape: "shapes",
  spec: "specs",
  trait: "traits",
  prompt: "prompts",
  provider: "providers",
  builtin: "builtins",
  policy: "policies",
  seed: "seeds",
};

/**
 * The catalog kind -> the keyword the run path knows it by.
 *
 * Only `mutation` differs, and the engine's own `constructKeyword` table says
 * the same thing. Written as a table rather than a special case so the one
 * difference is visible rather than buried in a conditional.
 */
const RUNNABLE_KIND_BY_CATALOG_KIND: Readonly<Record<string, RunnableKind>> = {
  query: "query",
  mutation: "mutate",
  logic: "logic",
  tool: "tool",
  automation: "automation",
};

/** A construct as this surface uses it: the wire record, typed for the run path. */
export interface CatalogConstruct {
  name: string;
  /** The kind the CATALOG reports (`mutation`, not `mutate`). */
  kind: string;
  namespace: string;
  origin: Construct["origin"];
  originPath: string;
  description: string;
  /**
   * Server-derived. READ THIS, never a set membership test on `kind` -- the
   * engine decides what is runnable and a client re-deriving it would be a
   * second opinion about the thing it is least able to check.
   */
  runnable: boolean;
  /** Present exactly when `runnable` and the kind maps; the run path's spelling. */
  runnableKind?: RunnableKind;
  args: RunnableArg[];
  boundConcept: string;
  sourceHash: string;
  /** Present only for a promoted construct, which has no file. */
  source: string;
  /**
   * What fires this construct, for an AUTOMATION (memql#3805).
   *
   * The automation run form is decided ENTIRELY by this, so its absence is not
   * a missing detail -- it is a form that cannot be drawn. Undefined for every
   * other kind, and for an automation the cluster reports no trigger for, which
   * is manual-run rather than "fires on nothing".
   */
  trigger?: RunnableTrigger;
}

/** Narrow one wire arg onto the shape the argument form binds. */
export function toRunnableArg(arg: ConstructArg): RunnableArg {
  const out: RunnableArg = {
    name: arg.name,
    type: narrowArgType(arg.type),
    required: arg.required,
  };
  if (arg.enum.length > 0) out.enum = [...arg.enum];
  if (arg.description !== "") out.description = arg.description;
  if (arg.autoInjected) out.autoInjected = true;
  return out;
}

function narrowArgType(value: string): RunnableArgType {
  return (RUNNABLE_ARG_TYPES as readonly string[]).includes(value)
    ? (value as RunnableArgType)
    : "any";
}

export function toCatalogConstruct(c: Construct): CatalogConstruct {
  const runnableKind = RUNNABLE_KIND_BY_CATALOG_KIND[c.kind];
  return {
    name: c.name,
    kind: c.kind,
    namespace: c.namespace,
    origin: c.origin,
    originPath: c.originPath,
    description: c.description,
    runnable: c.runnable,
    // Both conditions, not either: `runnable` is the engine's verdict and the
    // mapping is what this editor can act on. A construct the engine calls
    // runnable whose kind this client cannot map is one it must NOT offer to
    // run -- there is no target to build.
    ...(c.runnable && runnableKind !== undefined ? { runnableKind } : {}),
    args: c.args.map(toRunnableArg),
    boundConcept: c.boundConcept,
    sourceHash: c.sourceHash,
    source: c.source,
    // Carried through unchanged and only when present. The SDK already
    // declines to default it into existence, and re-adding an empty one here
    // would undo that distinction one layer further in.
    ...(c.trigger === undefined ? {} : { trigger: { ...c.trigger } }),
  };
}

// ---------------------------------------------------------------------------
// grouping
// ---------------------------------------------------------------------------

export interface NamespaceGroup {
  namespace: string;
  constructs: CatalogConstruct[];
}

export interface KindGroup {
  kind: string;
  label: string;
  /** Every construct of this kind, across namespaces. */
  count: number;
  /** True when the kind's constructs can be run. */
  runnable: boolean;
  namespaces: NamespaceGroup[];
}

/**
 * Group by kind, then by namespace.
 *
 * A KIND THE VOCABULARY DOES NOT KNOW STILL RENDERS, at the end, under its own
 * raw name. The alternative -- dropping it -- would make this view quietly
 * disagree with the cluster it is describing, and the case is not theoretical:
 * a client is expected to outlive several engine releases.
 *
 * A kind's `runnable` is true when ANY of its constructs is, rather than being
 * decided from the kind name. That keeps the one judgement -- "can this be
 * run" -- server-side even at the group level.
 */
export function groupByKind(constructs: readonly CatalogConstruct[]): KindGroup[] {
  const byKind = new Map<string, CatalogConstruct[]>();
  for (const c of constructs) {
    const bucket = byKind.get(c.kind);
    if (bucket === undefined) byKind.set(c.kind, [c]);
    else bucket.push(c);
  }

  const groups: KindGroup[] = [];
  for (const [kind, list] of byKind) {
    groups.push({
      kind,
      label: KIND_LABELS[kind] ?? kind,
      count: list.length,
      runnable: list.some((c) => c.runnableKind !== undefined),
      namespaces: groupByNamespace(list),
    });
  }

  return groups.sort((a, b) => {
    const ai = KIND_ORDER.indexOf(a.kind);
    const bi = KIND_ORDER.indexOf(b.kind);
    // An unknown kind sorts after every known one, and unknowns sort among
    // themselves by name so the order is stable between reads.
    if (ai !== bi) return (ai < 0 ? KIND_ORDER.length : ai) - (bi < 0 ? KIND_ORDER.length : bi);
    return a.kind.localeCompare(b.kind);
  });
}

/**
 * Group one kind's constructs by namespace.
 *
 * A PROMOTED construct has no namespace -- it was never authored into a domain
 * -- and rather than being hidden under an empty heading it groups under a
 * named one. That group is where a developer first meets the seeded-vs-trained
 * distinction, so it is worth it having a name.
 */
export function groupByNamespace(constructs: readonly CatalogConstruct[]): NamespaceGroup[] {
  const byNamespace = new Map<string, CatalogConstruct[]>();
  for (const c of constructs) {
    const key = c.namespace === "" ? PROMOTED_NAMESPACE : c.namespace;
    const bucket = byNamespace.get(key);
    if (bucket === undefined) byNamespace.set(key, [c]);
    else bucket.push(c);
  }
  return [...byNamespace.entries()]
    .map(([namespace, list]) => ({
      namespace,
      constructs: [...list].sort((a, b) => a.name.localeCompare(b.name)),
    }))
    .sort((a, b) => a.namespace.localeCompare(b.namespace));
}

/** What a construct with no authored domain groups under. */
export const PROMOTED_NAMESPACE = "(no namespace)";

// ---------------------------------------------------------------------------
// the states this view can be in
// ---------------------------------------------------------------------------

export type CatalogState =
  | { kind: "loading" }
  | { kind: "notConnected" }
  /** The cluster does not speak `ListConstructs`. Stated, never blank. */
  | { kind: "versionMismatch"; message: string }
  /** The read failed for some other reason; the engine's own words. */
  | { kind: "failed"; message: string }
  | { kind: "loaded"; groups: KindGroup[]; total: number };

/**
 * The phrase the SDK throws when the cluster answers with an envelope it does
 * not recognise, which is exactly what a cluster predating the message does.
 *
 * Matched on the SENTENCE because the SDK throws a plain Error -- there is no
 * typed error to catch. That is fragile in one direction only: if the wording
 * changes, this degrades to `failed`, which still renders the engine's own
 * message rather than a blank view. The acceptance it serves -- "never an
 * empty list, which reads as 'this cluster has no constructs'" -- holds either
 * way.
 */
const VERSION_MISMATCH_PHRASE = "unexpected reply envelope";

export function classifyCatalogFailure(err: unknown): CatalogState {
  const message = err instanceof Error ? err.message : String(err);
  if (message.includes(VERSION_MISMATCH_PHRASE)) {
    return {
      kind: "versionMismatch",
      message:
        "This cluster does not answer ListConstructs, so its constructs cannot be listed. " +
        "It is running an engine from before that message existed.",
    };
  }
  return { kind: "failed", message };
}

/** The catalog, grouped, or the state that explains why it is not here. */
export function catalogFrom(constructs: readonly Construct[]): CatalogState {
  const mapped = constructs.map(toCatalogConstruct);
  return { kind: "loaded", groups: groupByKind(mapped), total: mapped.length };
}
