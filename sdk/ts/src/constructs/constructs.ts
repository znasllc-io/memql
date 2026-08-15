// The construct catalog: what a cluster has actually LOADED, at registry
// grain (memql#3749 / #3750).
//
// This is not the pack browser. That one is FILE-grain -- domain, filenames,
// raw source -- and answers "show me this file". This answers "what do you
// have", and the difference is load-bearing in both directions: a PROMOTED
// construct lives in a database row and in no file, so no file walk can see
// it; a construct the loader SKIPPED sits in a file and not in the engine, so
// a file walk would list something the cluster cannot run.
//
// Three fields carry rules a consumer will get wrong by guessing.
//
//   `runnable` IS THE ANSWER. `kind` is the kind, not the authored keyword: a
//     mutation is authored `mutate` and reported here as "mutation". So
//     testing `RUNNABLE_KINDS.includes(kind)` fails on every mutation. The
//     engine derives `runnable` from the same five-kind set the extension
//     fixed; read it.
//
//   `origin` IS SERVER-DERIVED. core (the embedded tree) | bundle (a
//     runtime-mounted product domain, what MEMQL_DSL_PATH registers) |
//     promoted (trained into the running cluster). Deriving it a second way
//     client-side is a second definition of it.
//
//   `source` IS ONLY THERE WHEN THERE IS NO FILE. A promoted construct
//     carries its source because nothing else can serve it; a file-backed one
//     leaves it empty and is read through the pack browser at `originPath`.
//     So: source non-empty -> render it (there is no file to open); originPath
//     non-empty -> open that file; both empty -> the source is genuinely
//     unavailable, which is what a construct registered from Go looks like.
//
// `args` is the SAME shape the language server reports for
// `memql/runnableConstructs`, from the same analysis over the same source. One
// parser owns the argument form, so a form generated from this cannot disagree
// with the compiler about what a construct accepts. It is empty for every
// view-only kind -- an argument form implies a run.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type { ConstructArgWire, ConstructInfoWire, ConstructTriggerWire } from "../client/wire.js";

// -----------------------------------------------------------------------------
// Plain result types (no wire shapes leak to consumers)
// -----------------------------------------------------------------------------

/** A construct's origin. Derived server-side; never re-derived here. */
export type ConstructOrigin = "core" | "bundle" | "promoted";

/**
 * One declared input of a runnable construct.
 *
 * Field-for-field the language server's `RunnableArg`, so one argument-form
 * model serves both paths. `enum` is spelled `enum_values` on the wire only
 * because `enum` is reserved in the proto language; it is `enum` here.
 */
export interface ConstructArg {
  name: string;
  /** One of string | number | boolean | object | array | any. */
  type: string;
  required: boolean;
  /** The closed value set from `@enum(...)`; empty when unconstrained. */
  enum: string[];
  description: string;
  /**
   * A tool field the engine stamps server-side, DROPPING whatever the caller
   * sent. Only a tool field can carry it.
   */
  autoInjected: boolean;
}

/** One construct the cluster has loaded. */
export interface Construct {
  /** A concept's canonical id; the declared name for every other kind. */
  name: string;
  /**
   * concept | query | mutation | logic | tool | automation | spec | trait |
   * shape | prompt | provider | builtin | policy | seed.
   *
   * The KIND, not the authored keyword. Do not derive `runnable` from it.
   */
  kind: string;
  /** The DSL domain it was authored in. Empty for a promoted construct. */
  namespace: string;
  origin: ConstructOrigin;
  /** Tree-relative file path. Empty for a promoted construct. */
  originPath: string;
  description: string;
  /** One of the five runnable kinds. Server-derived; read this, not `kind`. */
  runnable: boolean;
  /** Always an array. Empty for every view-only kind. */
  args: ConstructArg[];
  /** The signature binding for query / mutation / shape / spec / seed. */
  boundConcept: string;
  /**
   * Canonical hash of the authored source, for drift detection. EMPTY means
   * "not available" -- never "hashes to nothing" -- so it must not be compared
   * as equal to another empty hash.
   */
  sourceHash: string;
  /** Present only when `originPath` is empty. See the module comment. */
  source: string;
  /**
   * What fires this construct, for an AUTOMATION. Undefined for every other
   * kind, and for an automation the cluster reports no trigger for.
   *
   * UNDEFINED IS NOT AN EMPTY TRIGGER. An automation with no trigger is
   * manual-run, and its form says so from the absence; an empty object would
   * be a claim that it fires on nothing.
   */
  trigger?: ConstructTrigger;
}

/**
 * What fires an automation.
 *
 * Field for field the language server's own trigger shape, because the editor
 * builds ONE automation run form -- from the language server when the .memql
 * file is open, from this catalog when browsing a cluster where it is not.
 *
 * The form is decided ENTIRELY by these three: a `schedule` with no `event`
 * offers a single "fire it now with an empty event" mode; an `event` WITH a
 * `concept` offers a row picker over that concept's rows plus a pasted
 * payload; an `event` with no `concept` offers the pasted payload alone,
 * because there is no row set to pick from.
 */
export interface ConstructTrigger {
  /**
   * The canonical concept id whose rows the run form's picker browses. Empty
   * when the trigger names no concept.
   */
  concept: string;
  /**
   * The structured event kind ("node.created") when the cluster's composed
   * topic decomposes, and the whole authored topic when it does not.
   */
  event: string;
  /** The cron expression of a time-driven trigger. Empty otherwise. */
  schedule: string;
}

export interface ListConstructsResult {
  constructs: Construct[];
}

export interface ConstructsCallOptions {
  signal?: AbortSignal;
}

// -----------------------------------------------------------------------------
// Client
// -----------------------------------------------------------------------------

export class ConstructsClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("ConstructsClient: dispatcher is required");
  }

  /**
   * Every construct the connected cluster has loaded, sorted by (kind,
   * namespace, name) so two reads of an unchanged cluster can be diffed.
   *
   * Read-only, and no filters by design: the surface that consumes this groups
   * client-side and needs all of it, and a filter parameter would put the kind
   * vocabulary in a second place.
   *
   * A cluster predating the message answers with an envelope this does not
   * recognise, which throws -- a caller should render that as a stated version
   * mismatch naming `ListConstructs`, never as an empty catalog.
   */
  async listConstructs(
    opts: ConstructsCallOptions = {},
  ): Promise<ListConstructsResult> {
    const requestId = newShortId();
    const reply = await this.dispatcher.sendAndWait(
      { listConstructs: { requestId } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(
        `listConstructs: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind !== "listConstructsResult") {
      throw new Error("listConstructs: unexpected reply envelope");
    }
    return {
      constructs: (payload.value.constructs ?? []).map(constructFromWire),
    };
  }
}

// -----------------------------------------------------------------------------
// Wire -> plain
// -----------------------------------------------------------------------------

// Every field is defaulted rather than passed through: protojson omits a zero
// int32, an empty string, a false bool and an empty list entirely, so a
// consumer reading them raw gets `undefined` for exactly the common case.
function constructFromWire(c: ConstructInfoWire): Construct {
  return {
    name: c.name ?? "",
    kind: c.kind ?? "",
    namespace: c.namespace ?? "",
    origin: originFromWire(c.origin),
    originPath: c.originPath ?? "",
    description: c.description ?? "",
    runnable: c.runnable === true,
    args: (c.args ?? []).map(argFromWire),
    boundConcept: c.boundConcept ?? "",
    sourceHash: c.sourceHash ?? "",
    source: c.source ?? "",
    ...(c.trigger === undefined ? {} : { trigger: triggerFromWire(c.trigger) }),
  };
}

// The one field NOT defaulted into existence, and deliberately: absence is
// meaningful here in a way it is not for a string or a list. Defaulting it to
// an empty ConstructTrigger would turn "this construct has no trigger" into
// "this construct fires on nothing", and the run form reads manual-run off the
// absence. Its three MEMBERS are defaulted as everything else is, because once
// a trigger exists protojson still omits its empty strings.
function triggerFromWire(t: ConstructTriggerWire): ConstructTrigger {
  return {
    concept: t.concept ?? "",
    event: t.event ?? "",
    schedule: t.schedule ?? "",
  };
}

// An unknown origin resolves to "core" rather than being passed through as an
// arbitrary string: the field is a closed set the engine owns, and a consumer
// switching on it should never have to carry a default branch. A value outside
// the set means the cluster is newer than this client, and rendering a
// construct as core is the least wrong of the three.
function originFromWire(v: string | undefined): ConstructOrigin {
  return v === "bundle" || v === "promoted" ? v : "core";
}

function argFromWire(a: ConstructArgWire): ConstructArg {
  return {
    name: a.name ?? "",
    type: a.type ?? "any",
    required: a.required === true,
    enum: a.enumValues ?? [],
    description: a.description ?? "",
    autoInjected: a.autoInjected === true,
  };
}
