import { useCallback, useEffect, useMemo, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";

// The live model catalog (epic memql#4676, task memql#4683).
//
// ===========================================================================
// REQUEST/REPLY WITH AN EXPLICIT REFRESH, NOT A SUBSCRIPTION
// ===========================================================================
// The same deliberate downgrade useProviders.ts documents, and for the same
// reason: `fleetModels` projects live worker registrations rather than rows
// anyone writes, so there is no catalog row to subscribe to. The registrations
// UNDERNEATH it do carry broadcast events, which is why the machines page is
// live -- but a catalog re-derived on every heartbeat would re-render this
// page every fifteen seconds per machine to show the same answer.
//
// ===========================================================================
// NO SECOND ELIGIBILITY IMPLEMENTATION LIVES HERE
// ===========================================================================
// Everything this file does is RENDER what the server computed. Which machines
// are online, which model meets the capability profile, whether a machine is
// busy -- all of it arrives decided. A client that re-derived any of it would
// eventually disagree with the router, and the disagreement presents as a page
// promising a model that every call to it parks on.

export interface FleetMachineRow {
  registrationId: string;
  name: string;
  displayName: string;
  label: string;
  runtimes: string[];
  online: boolean;
  busy: boolean;
  activeCount: number;
  maxConcurrent: number;
}

export interface FleetModelRow {
  modelId: string;
  contextWindow: number;
  structuredOutput: boolean;
  embeddings: boolean;
  online: boolean;
  machineCount: number;
  onlineCount: number;
  machines: FleetMachineRow[];
}

export interface FleetModelsState {
  models: FleetModelRow[];
  loading: boolean;
  error: string;
  reload: () => void;
}

type RowBag = Record<string, unknown>;

function str(row: RowBag, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function bool(row: RowBag, key: string): boolean {
  const v = row[key];
  if (typeof v === "boolean") return v;
  return typeof v === "string" && v.toLowerCase() === "true";
}

function num(row: RowBag, key: string): number {
  const v = row[key];
  if (typeof v === "number") return v;
  if (typeof v === "string" && v.trim() !== "") {
    const parsed = Number(v);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

function strList(row: RowBag, key: string): string[] {
  const v = row[key];
  if (!Array.isArray(v)) return [];
  return v.filter((x): x is string => typeof x === "string");
}

// materialize absorbs both shapes a caller can hand back: the SDK's Result,
// and a plain array (what a component test constructs). Copied in spirit from
// useProviders for the reason it exists there -- a page test should not have to
// build an SDK Result to say what the server returned.
export function materialize(result: unknown): RowBag[] {
  if (Array.isArray(result)) return result as RowBag[];
  const bag = result as { rows?: () => unknown } | null;
  if (bag && typeof bag.rows === "function") {
    const rows = bag.rows();
    if (Array.isArray(rows)) return rows as RowBag[];
  }
  return [];
}

export function toFleetModels(rows: readonly RowBag[]): FleetModelRow[] {
  return rows.map((row) => ({
    modelId: str(row, "modelId"),
    contextWindow: num(row, "contextWindow"),
    structuredOutput: bool(row, "structuredOutput"),
    embeddings: bool(row, "embeddings"),
    online: bool(row, "online"),
    machineCount: num(row, "machineCount"),
    onlineCount: num(row, "onlineCount"),
    machines: (Array.isArray(row.machines) ? (row.machines as RowBag[]) : []).map((m) => {
      const displayName = str(m, "displayName");
      const name = str(m, "name");
      return {
        registrationId: str(m, "registrationId"),
        name,
        displayName,
        // ONE derivation of what a machine is called, matching rows.ts's
        // `label`, so a model's machine list and the machines page cannot
        // disagree about the name of the same laptop.
        label: displayName.trim() !== "" ? displayName : name,
        runtimes: strList(m, "runtimes"),
        online: bool(m, "online"),
        busy: bool(m, "busy"),
        activeCount: num(m, "activeCount"),
        maxConcurrent: num(m, "maxConcurrent"),
      };
    }),
  }));
}

// summarizeFleet is the headline, as a pure function so the states can be
// tested without rendering.
//
// "FULLY LOCAL" IS A THING THE PAGE SAYS OUT LOUD, because it is the outcome
// this whole epic is for and an operator has no other way to confirm it. It is
// only claimed when a cloud provider is genuinely absent -- saying it while a
// key sits configured would be a claim about spend that is not true.
export function summarizeFleet(
  models: readonly FleetModelRow[],
  cloudConfigured: boolean,
): { tone: "none" | "asleep" | "local" | "mixed"; headline: string } {
  const total = models.length;
  const online = models.filter((m) => m.online).length;

  if (total === 0) {
    return {
      tone: "none",
      headline: cloudConfigured
        ? "No machine in your fleet is offering a model. This cluster runs on its cloud providers."
        : "No machine in your fleet is offering a model, and no cloud provider is configured.",
    };
  }
  if (online === 0) {
    return {
      tone: "asleep",
      headline: `${total} local ${total === 1 ? "model is" : "models are"} known, but no machine offering ${total === 1 ? "it" : "them"} is awake right now.`,
    };
  }
  if (!cloudConfigured) {
    return {
      tone: "local",
      headline: `This cluster is running fully local: ${online} of ${total} ${total === 1 ? "model" : "models"} available on your own machines, and no cloud provider configured.`,
    };
  }
  return {
    tone: "mixed",
    headline: `${online} of ${total} local ${total === 1 ? "model is" : "models are"} available, alongside this cluster's cloud providers.`,
  };
}

// chainInWords renders a policy's provider chain the way the policy editor
// shows it: "planner: local llama3.1:8b -> park".
//
// PARK IS THE TERMINAL STEP WHEN NO FALLBACK IS AUTHORED, and rendering it is
// the point. A chain shown as just "local llama3.1:8b" leaves the reader to
// assume what happens when the laptop closes, and the assumption everyone
// makes is "it falls back to the cloud" -- which is exactly what does NOT
// happen, and the reason a plan they are waiting on is parked.
export function chainInWords(chain: readonly string[]): string {
  if (chain.length === 0) return "not configured";
  const steps = chain.map((entry) =>
    entry.startsWith("fleet:") ? `local ${entry.slice("fleet:".length)}` : entry,
  );
  if (chain.every((entry) => entry.startsWith("fleet:"))) {
    steps.push("park");
  }
  return steps.join(" → ");
}

// formatContextWindow renders a token count the way an operator reads it. Zero
// is "not reported" rather than "0 tokens": the machine did not say, and a
// rendered zero is a claim it never made.
export function formatContextWindow(tokens: number): string {
  if (!Number.isFinite(tokens) || tokens <= 0) return "context not reported";
  if (tokens >= 1000) return `${Math.round(tokens / 1000)}k context`;
  return `${tokens} context`;
}

export function useFleetModels(enabled: boolean): FleetModelsState {
  const { query, status } = useCluster();
  const [models, setModels] = useState<FleetModelRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!enabled || !query || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    query
      .fleetModels({})
      .then((result: unknown) => {
        if (stale) return;
        setModels(toFleetModels(materialize(result)));
      })
      .catch((err: unknown) => {
        if (!stale) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
    };
  }, [enabled, query, status, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return useMemo(() => ({ models, loading, error, reload }), [models, loading, error, reload]);
}
