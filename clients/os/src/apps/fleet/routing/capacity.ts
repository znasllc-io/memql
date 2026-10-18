// Fleet capacity: whether a machine can take a call for a model RIGHT NOW
// (design record 2026-10-18-routes-design.md, "Fleet capacity: the layer
// under every route").
//
// The router admits a fleet machine only when the model has a free slot on it
// and the machine can serve the model without overcommitting memory. This
// file reads the same measurement (`fleetCapacity()`) so the routing page can
// say why a source cannot take calls in a few words -- "Busy, 2 of 2",
// "Needs 20 GB, 9 GB free" -- instead of drawing it ready.
//
// MISSING IS NOT ZERO. A machine that reports no memory is not a machine with
// no free memory, and a model with no slot figure is not a busy one: either
// absence yields no reason at all, and the source reads as it did before the
// measurement existed. The read is feature-detected the same way -- a cluster
// without the builtin has no capacity facts, and every source keeps its
// ordinary readiness.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";

export interface ModelCapacity {
  modelId: string;
  /** Calls running now, or null when not reported. */
  inFlight: number | null;
  /** Calls the machine takes at once for this model, or null when not reported. */
  slots: number | null;
  tokensPerSecond: number | null;
  medianLatencyMs: number | null;
  /** Memory the model needs to load, or null when not reported. */
  needBytes: number | null;
  loaded: boolean;
}

export interface MachineCapacity {
  machineId: string;
  name: string;
  memoryTotalBytes: number | null;
  memoryFreeBytes: number | null;
  /** "unified" | "vram" | "" when not said. */
  memoryKind: string;
  loaded: string[];
  models: ModelCapacity[];
}

export interface CapacityFacts {
  machines: MachineCapacity[];
}

function num(row: Row, ...keys: string[]): number | null {
  for (const key of keys) {
    const v = row[key];
    const n = typeof v === "number" ? v : typeof v === "string" && v.trim() !== "" ? Number(v) : NaN;
    if (Number.isFinite(n)) return n;
  }
  return null;
}

function text(row: Row, ...keys: string[]): string {
  for (const key of keys) {
    const v = row[key];
    if (typeof v === "string" && v.trim() !== "") return v.trim();
  }
  return "";
}

function list(v: unknown): unknown[] {
  return Array.isArray(v) ? v : [];
}

function bare(id: string): string {
  const at = id.lastIndexOf(":");
  return at === -1 ? id : id.slice(at + 1);
}

function modelFrom(raw: unknown, loaded: readonly string[]): ModelCapacity | null {
  if (raw === null || typeof raw !== "object") return null;
  const row = raw as Row;
  const modelId = text(row, "modelId", "model", "id");
  if (modelId === "") return null;
  return {
    modelId,
    inFlight: num(row, "inFlight", "active", "activeCount"),
    slots: num(row, "slots", "maxConcurrent", "concurrency"),
    tokensPerSecond: num(row, "tokensPerSecond", "tps"),
    medianLatencyMs: num(row, "medianLatencyMs", "latencyMs", "p50LatencyMs"),
    needBytes: num(row, "needBytes", "memoryNeedBytes"),
    loaded: row["loaded"] === true || loaded.includes(modelId),
  };
}

function machineFrom(raw: Row): MachineCapacity | null {
  const row = flatten(raw);
  const machineId = text(row, "machineId", "registrationId", "workerId", "id");
  if (machineId === "") return null;
  const memory = row["memory"] !== null && typeof row["memory"] === "object" ? (row["memory"] as Row) : row;
  const loaded = list(row["loaded"] ?? row["loadedModels"]).filter((m): m is string => typeof m === "string");
  return {
    machineId: bare(machineId),
    name: text(row, "displayName", "name") || bare(machineId),
    memoryTotalBytes: num(memory, "totalBytes", "memoryTotalBytes", "memoryTotal", "total"),
    memoryFreeBytes: num(memory, "freeBytes", "memoryFreeBytes", "memoryFree", "free"),
    memoryKind: text(memory, "kind", "memoryKind", "memoryType"),
    loaded,
    models: list(row["models"]).map((m) => modelFrom(m, loaded)).filter((m): m is ModelCapacity => m !== null),
  };
}

/**
 * The machines a `fleetCapacity` reply carries: one object with a `machines`
 * list, or one row per machine -- both read the same.
 */
export function capacityFrom(rows: readonly Row[]): CapacityFacts {
  const first = rows[0] === undefined ? null : flatten(rows[0]);
  const raw = first !== null && Array.isArray(first["machines"])
    ? (first["machines"] as unknown[]).filter((m): m is Row => m !== null && typeof m === "object")
    : [...rows];
  return { machines: raw.map(machineFrom).filter((m): m is MachineCapacity => m !== null) };
}

const GIB = 1024 ** 3;

/** Gigabytes in the words the router's reasons use: "20 GB", "9 GB", "0.6 GB". */
export function gigabytes(bytes: number): string {
  const gb = bytes / 1e9;
  return `${gb >= 10 ? Math.round(gb) : Math.round(gb * 10) / 10} GB`;
}

/**
 * Why this machine cannot take a call for this model now, or "" when it can
 * or nothing says otherwise.
 *
 * The router's admission, read for a person: a model with every slot in use
 * is busy; a model that is not loaded needs its memory plus a tenth (at least
 * a gigabyte) of headroom free. Lower-case, like every reading's reason, so
 * it joins a sentence; the board capitalizes it where it stands alone.
 */
export function capacityReason(capacity: CapacityFacts | null | undefined, machineId: string, modelId: string): string {
  if (!capacity) return "";
  const machine = capacity.machines.find((m) => m.machineId === bare(machineId));
  if (!machine) return "";
  const model = machine.models.find((m) => m.modelId === modelId);
  if (model && model.slots !== null && model.slots > 0 && model.inFlight !== null && model.inFlight >= model.slots) {
    return `busy, ${model.inFlight} of ${model.slots}`;
  }
  const loaded = machine.loaded.includes(modelId) || model?.loaded === true;
  const need = model?.needBytes ?? null;
  if (!loaded && need !== null && need > 0 && machine.memoryFreeBytes !== null) {
    const headroom = Math.max(need * 0.1, GIB);
    if (machine.memoryFreeBytes < need + headroom) return `needs ${gigabytes(need)}, ${gigabytes(Math.max(0, machine.memoryFreeBytes))} free`;
  }
  return "";
}

/** A model's measured speed on a machine, in few words, or "". */
export function capacitySpeed(capacity: CapacityFacts | null | undefined, machineId: string, modelId: string): string {
  const model = capacity?.machines.find((m) => m.machineId === bare(machineId))?.models.find((m) => m.modelId === modelId);
  if (!model || model.tokensPerSecond === null || model.tokensPerSecond <= 0) return "";
  return `${Math.round(model.tokensPerSecond)} tokens/s`;
}
