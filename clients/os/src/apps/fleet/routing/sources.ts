// Sources and their readiness, as pure functions of what the cluster reported.
//
// ===========================================================================
// READINESS IS READ, NEVER SIMULATED
// ===========================================================================
// The composer's circuit lights up to the first slot that is ready RIGHT NOW,
// and the owner asked for that moment to feel like a lock opening. A lock that
// opens on a guess is worse than no lock at all, so every "ready" here comes
// from a fact the cluster reported:
//
//   a local model   a machine that is online (its stream held, its heartbeat
//                   fresh -- `isWorkerOnline`) offers it in its `model:` labels
//   an app          a machine that is online reports it runnable (signed in,
//                   allowed by the machine, an app this engine drives)
//   a vendor        `inferenceStatus` says a vendor is set up
//   another route   something in ITS chain is ready
//
// THREE ANSWERS, NOT TWO. `unknown` is its own value: before the machines have
// been read, or for a source this surface cannot check (the active embeddings
// binding, a provider named directly), the honest answer is "not known" -- and
// the circuit stops there rather than lighting a slot behind it, because the
// router would try the unknown one first and it may well serve.

import { machineModelsFrom, type MachineModel } from "../machines/models";
import { orderModels } from "../models/ordering";
import { RUNNABLE_APPS, appLabel, type MachineApp } from "../rows";
import type { LabelMap } from "../labels";
import { routeTitle, sourceKind, sourceLabel, type SourceKind } from "./vocabulary";

export type Readiness = "ready" | "idle" | "unknown";

/** One machine, as far as routing cares. Revoked machines never get here. */
export interface MachineFacts {
  id: string;
  name: string;
  online: boolean;
  models: MachineModel[];
  apps: MachineApp[];
}

/** A route as far as another route's readiness cares: its name and its entries. */
export interface RouteLike {
  name: string;
  entries: readonly string[];
}

export interface RoutingFacts {
  /** The machine feed has settled; before that, nothing about a machine is known. */
  machinesRead: boolean;
  machines: readonly MachineFacts[];
  /** Whether a vendor is set up, per `inferenceStatus`. */
  vendor: Readiness;
  routes: readonly RouteLike[];
  /** The owner's model order, which `fleet:strongest` honours first. */
  preference: readonly string[];
}

export interface SourceReading {
  entry: string;
  label: string;
  kind: SourceKind;
  readiness: Readiness;
  /** One small line under the name: where it runs, which model. */
  fact: string;
  /** Why it cannot serve right now. Empty when ready or unknown. */
  reason: string;
  /** A model that only makes embeddings, which a chat route cannot use. */
  embeddingsOnly: boolean;
  /**
   * What ANSWERS when this serves: the source itself, or -- for another
   * route -- whatever that route would serve with. "Serves with Claude Code"
   * says where the call goes; "serves with Local first" would say only where
   * to look next.
   */
  serves: string;
}

/** What `factsFrom` needs of a machine row -- a projection, so tests need no row. */
export interface MachineInput {
  id: string;
  name: string;
  displayName: string;
  online: boolean;
  revoked: boolean;
  labels: LabelMap;
  apps: MachineApp[];
}

export function factsFrom(input: {
  machines: readonly MachineInput[];
  machinesRead: boolean;
  inference: { read: boolean; federationConfigured: boolean; error: string };
  routes: readonly RouteLike[];
  preference: readonly string[];
}): RoutingFacts {
  return {
    machinesRead: input.machinesRead,
    machines: input.machines
      .filter((m) => !m.revoked)
      .map((m) => ({
        id: m.id,
        name: m.displayName.trim() || m.name.trim() || m.id,
        online: m.online,
        models: machineModelsFrom(m.labels),
        apps: m.apps,
      })),
    vendor: !input.inference.read ? "unknown" : input.inference.federationConfigured ? "ready" : "idle",
    routes: input.routes,
    preference: input.preference,
  };
}

function reading(entry: string, readiness: Readiness, fact: string, reason: string, embeddingsOnly = false, serves = ""): SourceReading {
  const label = sourceLabel(entry);
  return { entry, label, kind: sourceKind(entry), readiness, fact, reason, embeddingsOnly, serves: readiness === "ready" ? serves || label : "" };
}

function offline(names: readonly string[]): string {
  return names.length === 1 ? `${names[0]} is offline` : "every machine with it is offline";
}

function chatModels(m: MachineFacts): MachineModel[] {
  return m.models.filter((model) => !model.embeddings);
}

/** Whether every machine offering this model says it only makes embeddings. */
function embeddingsOnlyModel(facts: RoutingFacts, modelId: string): boolean {
  const offers = facts.machines.flatMap((m) => m.models.filter((model) => model.modelId === modelId));
  return offers.length > 0 && offers.every((model) => model.embeddings);
}

function readLocalSelector(entry: string, selector: string, facts: RoutingFacts): SourceReading {
  const online = facts.machines.filter((m) => m.online);
  const models = online.flatMap((m) => chatModels(m).map((model) => ({ ...model, online: true })));
  if (models.length === 0) {
    const reason =
      facts.machines.length === 0
        ? "no machine connected"
        : online.length === 0
          ? "no machine online"
          : "no chat model on an online machine";
    return reading(entry, "idle", "", reason);
  }
  if (selector === "strongest") {
    const top = orderModels(models, facts.preference)[0]!;
    // What answers is the MODEL, so that is what "serves with" names.
    return reading(entry, "ready", top.modelId, "", false, top.modelId);
  }
  const count = new Set(models.map((m) => m.modelId)).size;
  return reading(entry, "ready", `${count} ${count === 1 ? "model" : "models"}`, "");
}

function readLocalModel(entry: string, modelId: string, facts: RoutingFacts): SourceReading {
  const holders = facts.machines.filter((m) => m.models.some((model) => model.modelId === modelId));
  const only = embeddingsOnlyModel(facts, modelId);
  if (holders.length === 0) return reading(entry, "idle", "", "not on any machine", only);
  const live = holders.find((m) => m.online);
  if (live) return reading(entry, "ready", `on ${live.name}`, "", only);
  return reading(entry, "idle", holders[0]!.name, offline(holders.map((m) => m.name)), only);
}

function readApp(entry: string, appId: string, facts: RoutingFacts): SourceReading {
  const holders = facts.machines.filter((m) => m.apps.some((a) => a.id === appId));
  if (holders.length === 0) return reading(entry, "idle", "", "not installed on any machine");
  const ready = holders.find((m) => m.online && m.apps.some((a) => a.id === appId && a.runnable));
  if (ready) return reading(entry, "ready", `on ${ready.name}`, "");
  const awake = holders.find((m) => m.online);
  if (awake) {
    const why = awake.apps.find((a) => a.id === appId)?.why || "not ready";
    return reading(entry, "idle", awake.name, `${why.charAt(0).toLowerCase()}${why.slice(1)} on ${awake.name}`);
  }
  return reading(entry, "idle", holders[0]!.name, offline(holders.map((m) => m.name)));
}

function readAnyApp(entry: string, facts: RoutingFacts): SourceReading {
  for (const m of facts.machines) {
    if (!m.online) continue;
    const app = m.apps.find((a) => a.runnable);
    if (app) return reading(entry, "ready", `${appLabel(app.id)} on ${m.name}`, "", false, appLabel(app.id));
  }
  return reading(entry, "idle", "", "no signed-in app on an online machine");
}

/**
 * One chain entry, read against the facts.
 *
 * `seen` guards a loop through routes: the engine refuses a cycle at save, but
 * a surface reading a half-written catalog must still terminate.
 */
export function readSource(entry: string, facts: RoutingFacts, seen: ReadonlySet<string> = new Set()): SourceReading {
  const kind = sourceKind(entry);
  const rest = entry.includes(":") ? entry.slice(entry.indexOf(":") + 1) : entry;

  if (kind === "vendor") {
    return reading(entry, facts.vendor, "", facts.vendor === "idle" ? "no vendor set up" : "");
  }
  if (kind === "embeddings") return reading(entry, "unknown", "", "", true);
  if (kind === "provider") return reading(entry, "unknown", "", "");
  if (kind === "route") {
    const route = facts.routes.find((r) => r.name === rest);
    if (!route) return reading(entry, "idle", "", "no such route");
    if (seen.has(rest)) return reading(entry, "idle", "", "loops back to this route");
    const next = new Set(seen).add(rest);
    const serving = servingIndex(route.entries, facts, next);
    if (serving.index >= 0) {
      const via = readSource(route.entries[serving.index]!, facts, next);
      return reading(entry, "ready", `via ${via.serves}`, "", false, via.serves);
    }
    if (serving.blockedAt >= 0) return reading(entry, "unknown", "", "");
    return reading(entry, "idle", "", "nothing in it is ready");
  }

  // Machines are the one fact that arrives late. Before the feed settles, a
  // local model or an app is not idle -- it is not known yet.
  if (!facts.machinesRead) return reading(entry, "unknown", "", "", kind === "local" && embeddingsOnlyModel(facts, rest));

  if (kind === "local") {
    if (rest === "strongest" || rest === "fastest") return readLocalSelector(entry, rest, facts);
    return readLocalModel(entry, rest, facts);
  }
  // kind === "app"
  if (rest === "*") return readAnyApp(entry, facts);
  const appId = rest.split(":")[0] ?? rest;
  return readApp(entry, appId, facts);
}

/**
 * Where the circuit stops: the first ready slot, or -1.
 *
 * `blockedAt` is the first slot whose readiness is not known, when it comes
 * before any ready one. The router tries that slot first and it may serve, so
 * nothing behind it is lit.
 */
export function servingIndex(
  entries: readonly string[],
  facts: RoutingFacts,
  seen: ReadonlySet<string> = new Set(),
): { index: number; blockedAt: number } {
  for (let i = 0; i < entries.length; i++) {
    const r = readSource(entries[i]!, facts, seen).readiness;
    if (r === "ready") return { index: i, blockedAt: -1 };
    if (r === "unknown") return { index: -1, blockedAt: i };
  }
  return { index: -1, blockedAt: -1 };
}

/**
 * A route's live status in few words.
 *
 * EMPTY while the machines have not been read and nothing is known yet: a
 * list draws a skeleton there rather than claiming "Nothing ready".
 */
export function routeStatus(entries: readonly string[], facts: RoutingFacts): { word: string; ready: boolean; serving: SourceReading | null } {
  if (entries.length === 0) return { word: "No sources", ready: false, serving: null };
  const at = servingIndex(entries, facts);
  if (at.index >= 0) {
    const serving = readSource(entries[at.index]!, facts);
    return { word: `Serves with ${serving.serves}`, ready: true, serving };
  }
  if (at.blockedAt >= 0) {
    return { word: facts.machinesRead ? "Not checked" : "", ready: false, serving: null };
  }
  return { word: "Nothing ready", ready: false, serving: null };
}

/** Whether a route carries chat calls or embeddings; embeddings is its own space. */
export function routeCarriesEmbeddings(name: string, entries: readonly string[]): boolean {
  return name === "embeddingsBinding" || entries.includes("embedder:active");
}

function includesRoute(facts: RoutingFacts, from: string, target: string, seen: Set<string> = new Set()): boolean {
  if (from === target) return true;
  if (seen.has(from)) return false;
  seen.add(from);
  const route = facts.routes.find((r) => r.name === from);
  if (!route) return false;
  return route.entries.some((e) => e.startsWith("policy:") && includesRoute(facts, e.slice("policy:".length), target, seen));
}

/** The engine's ceiling: a primary and sixteen fallbacks. */
export const MAX_SOURCES = 17;

/**
 * Why this source cannot go here, in one line -- or "" when it can.
 *
 * `at` is the slot being replaced, when the placement is a replacement: a
 * source that is already IN that slot is not a duplicate of itself.
 */
export function placementProblem(
  entry: string,
  entries: readonly string[],
  routeName: string,
  facts: RoutingFacts,
  at?: number,
): string {
  const others = entries.filter((_, i) => i !== at);
  if (others.includes(entry)) return "Already in this route.";
  if (at === undefined && entries.length >= MAX_SOURCES) return `A route holds at most ${MAX_SOURCES} sources.`;
  const kind = sourceKind(entry);
  if (kind === "route") {
    const target = entry.slice("policy:".length);
    if (routeName !== "" && target === routeName) return "A route cannot include itself.";
    if (routeName !== "" && includesRoute(facts, target, routeName)) return `${routeTitle(target)} already includes this route.`;
    const nested = facts.routes.find((r) => r.name === target);
    if (nested && routeCarriesEmbeddings(nested.name, nested.entries) && !routeCarriesEmbeddings(routeName, entries)) {
      return `${routeTitle(target)} only makes embeddings.`;
    }
    return "";
  }
  const embeddingsRoute = routeCarriesEmbeddings(routeName, entries);
  if (embeddingsRoute) return entry === "embedder:active" ? "" : "This route keeps the active embeddings only.";
  if (kind === "embeddings") return "Active embeddings only makes embeddings.";
  if (kind === "local") {
    const modelId = entry.slice("fleet:".length);
    if (embeddingsOnlyModel(facts, modelId)) return `${modelId} only makes embeddings.`;
  }
  return "";
}

export interface TrayGroup {
  id: "machines" | "choices" | "vendors" | "routes";
  title: string;
  sources: SourceReading[];
}

/**
 * Every source the cluster knows, grouped the way a person looks for one.
 *
 * NOTHING IS HIDDEN. Codex on no machine, a model on a machine that is asleep,
 * a vendor nobody set up: each stays in the tray, quiet, with its reason --
 * the person has to be able to see why a route would not connect.
 */
export function trayGroups(facts: RoutingFacts, routeName: string): TrayGroup[] {
  const appIds = [...RUNNABLE_APPS];
  const modelIds = Array.from(new Set(facts.machines.flatMap((m) => m.models.map((model) => model.modelId)))).sort();
  const machines = [
    ...appIds.map((id) => readSource(`app:${id}`, facts)),
    ...modelIds.map((id) => readSource(`fleet:${id}`, facts)),
  ];
  const choices = ["fleet:strongest", "fleet:fastest", "app:*"].map((e) => readSource(e, facts));
  const vendors = ["federation:cheapest", "federation:strongest"].map((e) => readSource(e, facts));
  const routes = facts.routes
    .filter((r) => r.name !== routeName)
    .map((r) => readSource(`policy:${r.name}`, facts));
  return [
    { id: "machines", title: "On your machines", sources: machines },
    { id: "choices", title: "Choices", sources: choices },
    { id: "vendors", title: "Vendors", sources: vendors },
    { id: "routes", title: "Routes", sources: routes },
  ];
}
