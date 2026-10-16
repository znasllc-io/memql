import { useGraphRevision } from "../../../live/useGraphRevision";
import { useCallback, useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import { boolOr, stringsOf } from "../../../kit";
import { useRoutingPolicy } from "../routing/useRoutingPolicy";
import type { RankedModel } from "./ordering";
import type { ModelProfile } from "./catalog";

// The Models section's two readings (epic memql#5096).
//
// ===========================================================================
// NEITHER IS LIVE, AND THAT IS THE HONEST ANSWER
// ===========================================================================
// `fleetModels` and `inferenceStatus` are VIRTUAL projections -- the
// `v1:router:modelCatalog` pattern -- computed per request from which machines
// are awake right now and never persisted. There is no `graph.node.*` event
// for either, so a live collection over one would render "Loading from the
// cluster" and then a list that never moves: the exact failure this app's
// workbench notes call out.
//
// So they read once, say WHEN they looked, and offer to look again. That is
// the Cluster app's rule, and its reasoning carries: a screen where half the
// panels move and half do not is worse than one where none do, because the
// reader cannot tell which kind they are looking at.
//
// ===========================================================================
// THEY SETTLE SEPARATELY
// ===========================================================================
// Deliberately not one combined await. The doors reading and the catalog
// reading have different reasons to fail -- one asks a registry, the other a
// fleet -- and a combined one lets the read that WILL be refused decide the
// state of the one that succeeded.

export interface DoorsReading {
  eligible: boolean;
  doorsOpen: string[];
  localEligible: boolean;
  localModelCount: number;
  eligibleModelIds: string[];
  appEligible: boolean;
  runnableApps: string[];
  appSessionsInstalled: boolean;
  cloudConfigured: boolean;
  federationConfigured: boolean;
  fleetInferenceInstalled: boolean;
  fleetCatalogInstalled: boolean;
  minimumContextWindow: number;
}

/** One catalog row, plus the machines behind it. */
export interface CatalogModel extends RankedModel {
  quant: string;
  machineCount: number;
  onlineCount: number;
  machines: CatalogMachine[];
}

export interface CatalogMachine {
  registrationId: string;
  name: string;
  displayName: string;
  runtimes: string[];
  online: boolean;
  busy: boolean;
  activeCount: number;
  maxConcurrent: number;
  /**
   * What this machine has to give a model, in whole gigabytes, as the engine's
   * UsableGigabytes computes it -- VRAM on a discrete card, 75 percent of the
   * pool on unified memory.
   *
   * ZERO MEANS THE MACHINE HAS NOT SAID, never a machine with no memory. A
   * cockpit that predates the hardware scanner sends no inventory at all, and a
   * reader that took the zero as a measurement would tell somebody their machine
   * is too small when nobody has asked it yet.
   */
  memoryGb: number;
  /** macos | linux, in the catalog's vocabulary; "" when the machine has not said. */
  platform: string;
}

function numberOf(row: Row | Record<string, unknown>, key: string): number {
  const raw = (row as Record<string, unknown>)[key];
  if (typeof raw === "number" && Number.isFinite(raw)) return raw;
  if (typeof raw === "string" && raw.trim() !== "") {
    const parsed = Number(raw);
    if (Number.isFinite(parsed)) return parsed;
  }
  return 0;
}

function stringOf(row: Row | Record<string, unknown>, key: string): string {
  const raw = (row as Record<string, unknown>)[key];
  return typeof raw === "string" ? raw : "";
}

/** Project one `v1:platform:fleetModel` row. */
export function catalogModelFromRow(row: Row): CatalogModel {
  const rawMachines = Array.isArray((row as Record<string, unknown>).machines)
    ? ((row as Record<string, unknown>).machines as Record<string, unknown>[])
    : [];
  return {
    // The row id IS the model id; `modelId` is on the payload too, and
    // preferring it means a row whose id was minted differently still reads
    // correctly.
    modelId: stringOf(row, "modelId") || stringOf(row, "id"),
    params: numberOf(row, "params"),
    activeParams: numberOf(row, "activeParams"),
    contextWindow: numberOf(row, "contextWindow"),
    structuredOutput: boolOr(row, "structuredOutput", false),
    embeddings: boolOr(row, "embeddings", false),
    tools: boolOr(row, "tools", false),
    online: boolOr(row, "online", false),
    quant: stringOf(row, "quant"),
    machineCount: numberOf(row, "machineCount"),
    onlineCount: numberOf(row, "onlineCount"),
    machines: rawMachines.map((m) => ({
      registrationId: stringOf(m, "registrationId"),
      name: stringOf(m, "name"),
      displayName: stringOf(m, "displayName"),
      runtimes: Array.isArray(m.runtimes) ? (m.runtimes as unknown[]).filter((r): r is string => typeof r === "string") : [],
      online: m.online === true,
      busy: m.busy === true,
      activeCount: numberOf(m, "activeCount"),
      maxConcurrent: numberOf(m, "maxConcurrent"),
      // Clamped at zero rather than passed through. A negative memory figure is
      // not a small machine, it is a machine that said something impossible, and
      // the readers downstream all treat "0" as "has not said" -- which is the
      // one reading that blocks nothing and claims nothing.
      memoryGb: Math.max(0, numberOf(m, "memoryGb")),
      platform: stringOf(m, "platform"),
    })),
  };
}

/** Project the single `v1:platform:inferenceStatus` row. */
export function doorsFromRow(row: Row | null): DoorsReading | null {
  if (row === null) return null;
  return {
    eligible: boolOr(row, "eligible", false),
    doorsOpen: stringsOf(row, "doorsOpen"),
    localEligible: boolOr(row, "localEligible", false),
    localModelCount: numberOf(row, "localModelCount"),
    eligibleModelIds: stringsOf(row, "eligibleModelIds"),
    appEligible: boolOr(row, "appEligible", false),
    runnableApps: stringsOf(row, "runnableApps"),
    appSessionsInstalled: boolOr(row, "appSessionsInstalled", false),
    cloudConfigured: boolOr(row, "cloudConfigured", false),
    federationConfigured: boolOr(row, "federationConfigured", false),
    fleetInferenceInstalled: boolOr(row, "fleetInferenceInstalled", false),
    fleetCatalogInstalled: boolOr(row, "fleetCatalogInstalled", false),
    minimumContextWindow: numberOf(row, "minimumContextWindow"),
  };
}

/** Project one `v1:models:modelProfile` row. */
export function modelProfileFromRow(row: Row): ModelProfile {
  return {
    modelId: stringOf(row, "modelId") || stringOf(row, "id"),
    category: stringOf(row, "category"),
    runtime: stringOf(row, "runtime"),
    family: stringOf(row, "family"),
    params: numberOf(row, "params"),
    quant: stringOf(row, "quant"),
    sizeBytes: numberOf(row, "sizeBytes"),
    contextWindow: numberOf(row, "contextWindow"),
    // `flags` is a LIST of the capability names that are true, deliberately the
    // same shape the `model:<id>` label uses -- so this surface compares a
    // profile against a machine field by field instead of translating between
    // two vocabularies that mean one thing.
    flags: stringsOf(row, "flags"),
    dimensions: numberOf(row, "dimensions"),
    license: stringOf(row, "license"),
    recommendedFor: stringsOf(row, "recommendedFor"),
    minMachineClass: stringOf(row, "minMachineClass"),
    offeredOn: stringsOf(row, "offeredOn"),
    notes: stringOf(row, "notes"),
    curated: boolOr(row, "curated", true),
    unavailable: boolOr(row, "unavailable", false),
  };
}

export function useInference() {
  const liveRevision = useGraphRevision(["v1:worker:registration", "v1:models:modelProfile", "v1:worker:delegationPolicy", "v1:worker:routingPolicy"]);
  const connection = useOsConnection();

  const readCatalog = useCallback(
    async (signal: AbortSignal): Promise<CatalogModel[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.fleetModels({}, { signal });
      return result.rows().map(catalogModelFromRow);
    },
    [connection],
  );

  const readDoors = useCallback(
    async (signal: AbortSignal): Promise<DoorsReading | null> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.inferenceStatus({}, { signal });
      return doorsFromRow(result.single());
    },
    [connection],
  );

  // THE CURATED CATALOG IS A THIRD READING, and it settles on its own for the
  // same reason the first two do (epic memql#5137, task memql#5140): it asks a
  // different question of a different source, and a combined await lets the
  // read that WILL be refused decide the state of the one that succeeded.
  //
  // It is also the only one of the three that is not a virtual projection --
  // `modelProfiles` reads real seeded rows -- so it is the one reading here
  // that would still answer on a cluster with no machines at all. That is the
  // point of it: it says what this fleet SHOULD run before anybody has pulled
  // anything.
  const readProfiles = useCallback(
    async (signal: AbortSignal): Promise<ModelProfile[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.modelProfiles({}, { signal });
      return result.rows().map(modelProfileFromRow);
    },
    [connection],
  );

  const catalog = useReading<CatalogModel[]>(
    `fleet:models:catalog:${liveRevision}`,
    connection === null ? null : readCatalog,
  );
  const profiles = useReading<ModelProfile[]>(
    `fleet:models:profiles:${liveRevision}`,
    connection === null ? null : readProfiles,
  );
  const doors = useReading<DoorsReading | null>(
    `fleet:models:doors:${liveRevision}`,
    connection === null ? null : readDoors,
  );

  // The owner's model preference, off the LIVE routing policy -- so an edit
  // in the Routing section re-ranks this list without a re-read. The policy
  // broadcasts; the catalog does not, which is why only one of the two moves
  // on its own.
  const routing = useRoutingPolicy();
  const preference = useMemo(
    () => routing.policy?.modelPreference ?? [],
    [routing.policy],
  );

  return { catalog, doors, profiles, preference, policyError: routing.error };
}
