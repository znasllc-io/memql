import { useCallback, useEffect, useState } from "react";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import { absent, figureFrom, type Figure } from "../../kit/measure";

// The three nouns, as facts rather than as prose (epic memql#5153, D1/D2).
//
// ===========================================================================
// THE DOOR ORDER IS THE PRODUCT, AND IT IS SPELLED ONCE
// ===========================================================================
// A call tries a model on a machine you own, then a subscription you already
// pay for, then a metered vendor. Paid inference is last BY CONSTRUCTION --
// the shipped policies chain in that order, and an owner who wants a vendor
// first has to say so in a rule of higher precedence, which every decision
// record then reports.
//
// Four screens have to say that, and if each spelled it there would be four
// orders the day one changed. `DOOR_ORDER` is the order and `doorReadings`
// is the only place a screen learns which door takes a call. Rule 7 -- say it
// once -- applied to a fact rather than to a caption.
//
// ===========================================================================
// WHAT THIS FILE REFUSES TO COMPUTE
// ===========================================================================
// It does NOT rank models within the fleet door, and it does not decide which
// model a level resolves to. That binding is the engine's (epic memql#5127),
// and a plausible client-side guess at it would be wrong in the one direction
// that matters: it would read as a measurement. Where the engine has not
// answered, a level says which DOOR takes it -- which is honestly derivable
// from the doors' own open/shut state -- and says the model is the fleet's to
// choose. An absent answer is a value here, never a blank.

/** A door is a place inference can come from. Closed set, engine-aligned. */
export type DoorId = "fleet" | "app" | "anthropic" | "openai";

/**
 * The try order, and the reason the product exists.
 *
 * `fleet` and `app` cost the cluster nothing; `anthropic` and `openai` are
 * metered. The two vendors sit at the same rung -- neither is tried before
 * the other by this ordering; a rule decides between them.
 */
export const DOOR_ORDER: readonly DoorId[] = ["fleet", "app", "anthropic", "openai"];

/** The engine's three-value door vocabulary, which the wire also uses. */
export type DoorKind = "local" | "app" | "federation";

export function doorKindOf(id: DoorId): DoorKind {
  if (id === "fleet") return "local";
  if (id === "app") return "app";
  return "federation";
}

export const DOOR_NAMES: Record<DoorId, string> = {
  fleet: "Your machines",
  app: "Signed-in apps",
  anthropic: "Anthropic",
  openai: "OpenAI",
};

/**
 * What each door costs, in the words a person decides with.
 *
 * This is the sentence the owner asked to be unmissable: the first two doors
 * spend nothing, and the reason to open them is that the third one bills.
 */
export const DOOR_COST: Record<DoorId, string> = {
  fleet: "Runs on hardware you own. Nothing is billed.",
  app: "Spends a subscription you already pay for. Nothing is billed here.",
  anthropic: "Billed per call, which is why it is tried last.",
  openai: "Billed per call, which is why it is tried last.",
};

/** Whether a call through this door costs the cluster money. */
export function doorIsMetered(id: DoorId): boolean {
  return doorKindOf(id) === "federation";
}

export type DoorState = "open" | "shut" | "half" | "unknown";

/** The word beside a door's dot. */
export const DOOR_STATE_WORDS: Record<DoorState, string> = {
  open: "Open",
  shut: "Not set up",
  half: "Half set up",
  unknown: "Not read",
};

export interface DoorReading {
  id: DoorId;
  name: string;
  kind: DoorKind;
  state: DoorState;
  /** One line saying what is true of this door now. Never empty. */
  said: string;
  /** The engine's own sentence, when it gave one. Never invented. */
  detail: string;
  metered: boolean;
}

/**
 * What `inferenceStatus` reports, projected.
 *
 * `eligibleModelIds` and `minimumContextWindow` are carried even though no
 * door STATE turns on them, because the fleet door's sentence does: "2 of 3
 * models meet the 32,000-token floor" is the reading, and "2 models" is a
 * count wearing it. The floor is published by the engine
 * (`dsl/platform/concepts.memql`) precisely so a surface names the number it
 * checked rather than one of its own.
 */
export interface InferenceReading {
  read: boolean;
  eligible: boolean;
  streamingChatEligible?: boolean;
  doorsOpen: string[];
  localEligible: boolean;
  localModelCount: number;
  eligibleModelIds: string[];
  minimumContextWindow: number;
  appEligible: boolean;
  runnableApps: string[];
  appSessionsInstalled: boolean;
  federationConfigured: boolean;
  fleetInferenceInstalled: boolean;
  fleetCatalogInstalled: boolean;
  error: string;
}

export const UNREAD_INFERENCE: InferenceReading = {
  read: false,
  eligible: false,
  streamingChatEligible: undefined,
  doorsOpen: [],
  localEligible: false,
  localModelCount: 0,
  eligibleModelIds: [],
  minimumContextWindow: 0,
  appEligible: false,
  runnableApps: [],
  appSessionsInstalled: false,
  federationConfigured: false,
  fleetInferenceInstalled: false,
  fleetCatalogInstalled: false,
  error: "",
};

function str(row: Record<string, unknown>, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function num(row: Record<string, unknown>, key: string): number {
  const v = row[key];
  if (typeof v === "number") return v;
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}

function bool(row: Record<string, unknown>, key: string): boolean {
  return row[key] === true;
}

function strings(row: Record<string, unknown>, key: string): string[] {
  const v = row[key];
  if (!Array.isArray(v)) return [];
  return v.filter((one): one is string => typeof one === "string");
}

export function inferenceFrom(row: Row | null | undefined, error: string): InferenceReading {
  if (row === null || row === undefined) {
    return { ...UNREAD_INFERENCE, error };
  }
  const r = flatten(row as Record<string, unknown>);
  return {
    read: true,
    eligible: bool(r, "eligible"),
    streamingChatEligible: typeof r.streamingChatEligible === "boolean" ? r.streamingChatEligible : undefined,
    doorsOpen: strings(r, "doorsOpen"),
    localEligible: bool(r, "localEligible"),
    localModelCount: num(r, "localModelCount"),
    eligibleModelIds: strings(r, "eligibleModelIds"),
    minimumContextWindow: num(r, "minimumContextWindow"),
    appEligible: bool(r, "appEligible"),
    runnableApps: strings(r, "runnableApps"),
    appSessionsInstalled: bool(r, "appSessionsInstalled"),
    federationConfigured: bool(r, "federationConfigured"),
    fleetInferenceInstalled: bool(r, "fleetInferenceInstalled"),
    fleetCatalogInstalled: bool(r, "fleetCatalogInstalled"),
    error,
  };
}

/**
 * The sentence an unread door carries, and the two readings it keeps apart.
 *
 * "Nobody has asked yet" and "we asked and could not get an answer" look
 * identical on a page and are different facts -- and NEITHER of them is "the
 * door is shut", which is what a blank here would invite a reader to conclude.
 * `unableTo` names what could not be asked, because the reason a reading is
 * missing is what decides whether there is anything to do about it.
 */
function unreadSaid(status: InferenceReading, unableTo: string): string {
  return status.error === "" ? "The cluster has not answered yet." : unableTo;
}

/**
 * The fleet door.
 *
 * `localEligible` is the engine's own verdict and outranks a model count: a
 * fleet holding four models none of which qualify is NOT an open local door,
 * and saying "4 models" beside a shut door would read as a contradiction the
 * person has to resolve.
 *
 * Catalog access is separate from dispatch: a BFF reads the inventory while
 * an agent places calls. Missing inventory access means availability is
 * unknown; it does not establish that pairing a machine cannot help.
 *
 * THE FLOOR IS NAMED, NOT IMPLIED. "None big enough for the work this cluster
 * does" is a paraphrase of a published number; an operator deciding which
 * model to pull needs the number.
 */
function fleetDoor(status: InferenceReading): DoorReading {
  const base = { id: "fleet" as const, name: DOOR_NAMES.fleet, kind: "local" as const, metered: false };
  if (!status.read) {
    return {
      ...base,
      state: "unknown",
      said: unreadSaid(
        status,
        "We could not ask this cluster what your machines are offering, which is not the same as a fleet with nothing on it.",
      ),
      detail: status.error,
    };
  }
  const floor = status.minimumContextWindow.toLocaleString();
  const models = status.localModelCount;
  const eligible = status.eligibleModelIds.length;
  if (status.localEligible) {
    return {
      ...base,
      state: "open",
      said: `${eligible} of ${models} ${models === 1 ? "model" : "models"} on your machines ${eligible === 1 ? "meets" : "meet"} the ${floor}-token floor, so a call can go there instead of to a vendor.`,
      detail: "",
    };
  }
  if (!status.fleetCatalogInstalled) {
    return {
      ...base,
      state: "unknown",
      said:
        "The fleet inventory cannot be read here, so model availability is unknown. Try reading again.",
      detail: "",
    };
  }
  if (models === 0) {
    return {
      ...base,
      state: "shut",
      said: "No machine you own is offering a model. This is the source that costs nothing.",
      detail: "",
    };
  }
  return {
    ...base,
    state: "half",
    said:
      models === 1
        ? `Your machines offer one model, and it does not meet the ${floor}-token floor with structured output. Pull a larger one.`
        : `Your machines offer ${models} models, and none of them meets the ${floor}-token floor with structured output. Pull a larger one.`,
    detail: "",
  };
}

/**
 * The app door: a signed-in Claude Code or Codex on a machine you own.
 *
 * `appSessionsInstalled` IS THE TWIN of `fleetInferenceInstalled`, and the
 * engine says so in its own field description: it reports whether the node
 * answering can open app sessions AT ALL, not whether an app is installed on
 * anybody's machine. So it is what tells "you have signed in nowhere" (half:
 * this side is ready, the person's is not) from "this node has no worker
 * service" (shut: nothing the person does to their laptop changes it).
 */
function appDoor(status: InferenceReading): DoorReading {
  const base = { id: "app" as const, name: DOOR_NAMES.app, kind: "app" as const, metered: false };
  if (!status.read) {
    return {
      ...base,
      state: "unknown",
      said: unreadSaid(
        status,
        "We could not ask this cluster which apps are signed in, which is not the same as none being signed in.",
      ),
      detail: status.error,
    };
  }
  if (status.appEligible && status.runnableApps.length > 0) {
    return {
      ...base,
      state: "open",
      said: `${appList(status.runnableApps)} signed in and ready.`,
      detail: "",
    };
  }
  if (status.appSessionsInstalled) {
    return {
      ...base,
      state: "half",
      said: "No signed-in Claude Code or Codex on any machine you own. This node can open a session as soon as there is one.",
      detail: "",
    };
  }
  return {
    ...base,
    state: "shut",
    said:
      "This cluster is not set up to run work inside a signed-in app, so signing in on a machine would not make one a source. That is a deployment setting rather than anything to fix here.",
    detail: "",
  };
}

const APP_NAMES: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
};

/** App ids as people say them. An unknown id is reported, never dropped. */
export function appList(ids: readonly string[]): string {
  const named = ids.map((id) => APP_NAMES[id] ?? id);
  if (named.length === 0) return "No app";
  if (named.length === 1) return named[0]!;
  if (named.length === 2) return `${named[0]} and ${named[1]}`;
  return `${named.slice(0, -1).join(", ")} and ${named[named.length - 1]}`;
}

/**
 * The four doors in try order.
 *
 * `vendorDoor` is handed in rather than derived here, because the vendor half
 * is `providerFacts.doorFor` and already reads the provider registry: two
 * readings of the same rows would be two places to disagree.
 */
export function doorReadings(
  status: InferenceReading,
  vendorDoor: (vendor: string) => { state: "open" | "unset" | "half" | "unknown"; said: string },
): DoorReading[] {
  const vendor = (id: "anthropic" | "openai"): DoorReading => {
    const d = vendorDoor(id);
    const state: DoorState = d.state === "unset" ? "shut" : d.state;
    return {
      id,
      name: DOOR_NAMES[id],
      kind: "federation",
      metered: true,
      state,
      // HALF IS THE ONE THAT SHOUTS, and the reason is the whole of it: the
      // engine REFUSES BOOT on a partial set, hours after the save that
      // caused it, so the consequence is named rather than left as "shut".
      // SHUT SAYS IT IS NORMAL, for the opposite reason: it is how every
      // cluster is installed and the permanent state of every local one, and
      // an operator who reads a fresh install as a fault concludes the
      // install failed.
      said:
        state === "open"
          ? "Used when a route reaches a vendor."
          : state === "half"
            ? "Some ids are set and some are missing, and a node that reads a partial set refuses to boot."
            : state === "unknown"
              ? "The cluster has not answered yet."
              : "Not set up, which is the normal state of a new cluster. Nothing here is billed until it is.",
      detail: d.said,
    };
  };
  return [fleetDoor(status), appDoor(status), vendor("anthropic"), vendor("openai")];
}

/** The first door that would take a call, or null when every door is shut. */
export function firstOpenDoor(doors: readonly DoorReading[]): DoorReading | null {
  return doors.find((d) => d.state === "open") ?? null;
}

// ===========================================================================
// LEVELS
// ===========================================================================

/** How much intelligence a call needs. Closed set of four (epic memql#5127). */
export type LevelId = "fast" | "strong" | "reasoning" | "embeddings";

export const LEVELS: readonly LevelId[] = ["fast", "strong", "reasoning", "embeddings"];

/**
 * What each level is for, in one clause.
 *
 * These teach the abstraction, which is the whole job of the Levels screen --
 * a call names a level and never a model, so the level is the only word a
 * person has to hold.
 */
export const LEVEL_MEANING: Record<LevelId, string> = {
  fast: "short turns where waiting is the cost",
  strong: "most real work",
  reasoning: "planning, and problems worth thinking about",
  embeddings: "turning text into vectors for search and memory",
};

export interface LevelReading {
  id: LevelId;
  meaning: string;
  /** The door that would take this level now, or null when none is open. */
  door: DoorReading | null;
  /** The model, when the engine has bound one. Empty means it has not. */
  model: string;
  /** Where the model runs, when known. */
  where: string;
  /** A measured figure for this level's model, or the reason there is none. */
  measured: Figure;
  /** True when no door has ANSWERED, as against every door being shut. */
  unread: boolean;
  /** The row's sentence, already assembled. */
  sentence: string;
  /** What to do about it, when the answer costs money or parks. Else empty. */
  advice: string;
}

/**
 * `embeddings` never degrades, at any setting: a degraded embedder answers in
 * a DIFFERENT VECTOR SPACE, so the vector does not belong in the index it is
 * about to be written to and every later similarity read comes back plausible
 * and wrong. The screen says so, because it is the one level whose failure is
 * silent.
 */
export const EMBEDDINGS_NEVER_DEGRADES =
  "Never degrades. A smaller embedder answers in a different vector space, so a " +
  "fallback would poison the index rather than slow it down.";

/**
 * Read the four levels.
 *
 * `bound` is the engine's level-to-model binding when it exists (epic
 * memql#5127's read). Absent, every level still says which DOOR takes it,
 * which is honestly derivable from the doors' own state, and says the model is
 * the fleet's to choose rather than guessing one.
 */
export function levelReadings(
  doors: readonly DoorReading[],
  bound: Partial<Record<LevelId, { model: string; where: string; measured?: Figure }>> = {},
): LevelReading[] {
  const open = firstOpenDoor(doors);
  // "NOTHING IS OPEN" AND "WE DID NOT GET AN ANSWER" ARE DIFFERENT ANSWERS.
  //
  // `firstOpenDoor` returns null for both, and reading the null as the first
  // one is the exact failure this epic exists to prevent -- it states a fact
  // about somebody's cluster, in the ink of a measurement, derived from a read
  // that never landed, and then advises them to go and buy hardware. An admin
  // whose `inferenceStatus` read was refused would meet it every time.
  const unread = open === null && doors.some((d) => d.state === "unknown");
  return LEVELS.map((id) => {
    const b = bound[id];
    const door = open;
    const model = b?.model ?? "";
    const where = b?.where ?? "";
    const measured = b?.measured ?? absent("unmeasured");
    return {
      id,
      meaning: LEVEL_MEANING[id],
      door,
      model,
      where,
      measured,
      unread,
      sentence: unread
        ? "The cluster has not said which sources are ready, so this level's answer is not known."
        : levelSentence(id, door, model, where),
      // NO ADVICE ON AN UNREAD CLUSTER. There is nothing to advise: the thing
      // to fix might be the read itself, and telling somebody to pull a model
      // because a query was refused sends them a long way from the problem.
      advice: unread ? "" : levelAdvice(id, door),
    };
  });
}

function levelSentence(id: LevelId, door: DoorReading | null, model: string, where: string): string {
  if (door === null) {
    return id === "embeddings"
      ? "No source is ready, so nothing can be embedded and the work waits rather than being written wrong."
      : "No source is ready, so a call at this level waits until one is.";
  }
  const named = model === "" ? "" : where === "" ? ` ${model}` : ` ${model} on ${where}`;
  if (door.kind === "local") {
    return model === ""
      ? "Your machines take this. The fleet picks the model."
      : `Your machines take this:${named}.`;
  }
  if (door.kind === "app") {
    return model === ""
      ? "No model on your fleet, so a signed-in app takes this. Your subscription, nothing billed here."
      : `A signed-in app takes this:${named}. Your subscription, nothing billed here.`;
  }
  return model === ""
    ? `Nothing local and no signed-in app, so this goes to ${door.name} and is billed.`
    : `Nothing local and no signed-in app, so this goes to ${door.name}:${named}. Billed.`;
}

/**
 * What to do about an answer that costs money or parks.
 *
 * Only rendered where there IS something to do -- an empty string on the three
 * happy cases, because advice attached to a working state is furniture.
 */
function levelAdvice(id: LevelId, door: DoorReading | null): string {
  if (door === null) {
    return id === "embeddings"
      ? "Pull an embedding model onto a machine you own."
      : "Pull a model onto a machine you own, or sign in to Claude Code or Codex on one.";
  }
  if (door.kind === "local") return "";
  if (door.kind === "app") {
    return "Pull a model onto a machine you own and this stops depending on an app being awake.";
  }
  return "Pull a model onto a machine you own, or sign in to Claude Code or Codex on one, and this stops being billed.";
}

// ===========================================================================
// THE READS
// ===========================================================================

/**
 * `inferenceStatus`, read once with an explicit re-read.
 *
 * It is a virtual projection with no `graph.node.*` event, so there is nothing
 * to subscribe to -- the same reason `models/useInference.ts` reads it this
 * way rather than through a live collection.
 */
export function useInferenceStatus(enabled: boolean): {
  status: InferenceReading;
  loading: boolean;
  fetchedAt: number | null;
  reload: () => void;
} {
  const connection = useOsConnection();
  const [status, setStatus] = useState<InferenceReading>(UNREAD_INFERENCE);
  const [loading, setLoading] = useState(false);
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    void connection.query
      .inferenceStatus({}, { signal: controller.signal })
      .then((result) => {
        if (stale) return;
        const rows = [...result.rows()];
        setStatus(inferenceFrom(rows[0], ""));
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (stale) return;
        setStatus(inferenceFrom(null, err instanceof Error ? err.message : String(err)));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled, epoch]);

  return { status, loading, fetchedAt, reload };
}

/**
 * The engine's level bindings, when the cluster has them.
 *
 * ABSENT ON `main` BY DESIGN. The read lands with epic memql#5127; until then
 * every cluster answers "no such query" and this resolves to an empty map,
 * which `levelReadings` renders as "the fleet picks the model" rather than as
 * a broken screen. The failure is therefore indistinguishable from a cluster
 * that simply has not bound a level yet, which is the correct reading of both.
 */
export function useLevelBindings(enabled: boolean): {
  bound: Partial<Record<LevelId, { model: string; where: string; measured?: Figure }>>;
  available: boolean;
} {
  const connection = useOsConnection();
  const [bound, setBound] = useState<
    Partial<Record<LevelId, { model: string; where: string; measured?: Figure }>>
  >({});
  const [available, setAvailable] = useState(false);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const q = connection.query as unknown as Record<
      string,
      ((args: Record<string, unknown>, opts?: unknown) => Promise<{ rows: () => Iterable<Row> }>) | undefined
    >;
    const read = q["routerLevelBindings"];
    if (typeof read !== "function") {
      setAvailable(false);
      return;
    }
    const controller = new AbortController();
    let stale = false;
    void read({}, { signal: controller.signal })
      .then((result) => {
        if (stale) return;
        const next: Partial<Record<LevelId, { model: string; where: string; measured?: Figure }>> = {};
        for (const raw of result.rows()) {
          const r = flatten(raw as Record<string, unknown>);
          const id = str(r, "level") as LevelId;
          if (!LEVELS.includes(id)) continue;
          next[id] = {
            model: str(r, "model"),
            where: str(r, "machine"),
            measured: figureFrom(r, "structuredValidity"),
          };
        }
        setBound(next);
        setAvailable(true);
      })
      .catch(() => {
        if (!stale) setAvailable(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled]);

  return { bound, available };
}
