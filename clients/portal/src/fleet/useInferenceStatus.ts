import { useCallback, useEffect, useMemo, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { materialize } from "./useFleetModels";

// "Can this person get inference at all, and through which door" (epic
// memql#4676, task memql#4684, design G).
//
// ===========================================================================
// ELIGIBILITY HAS EXACTLY ONE IMPLEMENTATION, AND IT IS NOT HERE
// ===========================================================================
// This file DECODES an answer. It does not compute one. The server's
// `inferenceStatus` reads the same catalog and provider registry the router
// reads, so a gate that let somebody through and a router that then refused
// every call cannot disagree -- which is the failure a client-side eligibility
// check produces, and it produces it silently: the console looks fine and
// every feature in it parks.
//
// Every input the server used is on the row anyway, so a person looking at a
// gate they do not understand can see what it read.

export type InferenceDoor = "local" | "federation" | "apiKey";

export interface InferenceStatus {
  eligible: boolean;
  doorsOpen: InferenceDoor[];
  localEligible: boolean;
  localModelCount: number;
  eligibleModelIds: string[];
  cloudConfigured: boolean;
  federationConfigured: boolean;
  // fleetInferenceInstalled distinguishes "your machines are asleep" from
  // "the node answering has no worker service". Identical from a page,
  // entirely different fixes.
  fleetInferenceInstalled: boolean;
  minimumContextWindow: number;
}

export interface InferenceStatusState {
  status: InferenceStatus | null;
  loading: boolean;
  error: string;
  reload: () => void;
}

type RowBag = Record<string, unknown>;

const DOORS: InferenceDoor[] = ["local", "federation", "apiKey"];

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

export function toInferenceStatus(rows: readonly RowBag[]): InferenceStatus | null {
  const row = rows[0];
  if (!row) return null;
  const doors = strList(row, "doorsOpen").filter((d): d is InferenceDoor =>
    (DOORS as string[]).includes(d),
  );
  return {
    eligible: bool(row, "eligible"),
    doorsOpen: doors,
    localEligible: bool(row, "localEligible"),
    localModelCount: num(row, "localModelCount"),
    eligibleModelIds: strList(row, "eligibleModelIds"),
    cloudConfigured: bool(row, "cloudConfigured"),
    federationConfigured: bool(row, "federationConfigured"),
    fleetInferenceInstalled: bool(row, "fleetInferenceInstalled"),
    minimumContextWindow: num(row, "minimumContextWindow"),
  };
}

// gateStep is the ordered first-run decision (design D9): passkey, then
// inference, then the console.
//
// ===========================================================================
// AUTH-DISABLED IS THE ONLY MODE WITH A SKIP, AND IT IS NOT A CONVENIENCE
// ===========================================================================
// With MEMQL_IDENTITY_ENABLED=false every stream is admitted as a synthetic
// cluster owner. There is no identity, so there is no passkey to enrol -- and
// the mode already tells the operator they are not really signed in. A gate
// that could not be passed in a troubleshooting mode would lock somebody out
// of the surface they were troubleshooting with.
//
// ===========================================================================
// A LATER OFFLINE MACHINE IS A NOTICE, NEVER AN EVICTION
// ===========================================================================
// `alreadyEntered` is what makes that true. Once a person is in the console,
// this returns "console" regardless of what the catalog says next --
// inference-needing features park with their own typed refusals (memql#4682)
// and a rail notice says why. Ejecting somebody mid-session because a laptop
// closed would be the portal punishing them for a thing that fixes itself.
export function gateStep(input: {
  authEnabled: boolean;
  hasPasskey: boolean;
  status: InferenceStatus | null;
  alreadyEntered: boolean;
  skipped: boolean;
  // unreadable is set when the status READ FAILED, which is different from it
  // not having answered yet.
  unreadable?: boolean;
}): "passkey" | "inference" | "console" {
  if (input.alreadyEntered) return "console";
  if (input.authEnabled && !input.hasPasskey) return "passkey";
  if (input.status?.eligible) return "console";
  // AN UNANSWERABLE QUESTION IS NOT A CLOSED DOOR. A read that ERRORED -- an
  // engine older than this construct, a transient failure, a node that
  // refused -- must not lock somebody out of their own console. The gate
  // exists to help a person configure inference, not to be a wall an
  // unrelated outage can raise; and a portal that becomes unusable because a
  // projection failed is a much worse outcome than one that skips a prompt.
  // Features needing a model still refuse and say so.
  if (input.unreadable) return "console";
  if (!input.authEnabled && input.skipped) return "console";
  // A STATUS THAT HAS NOT ANSWERED YET LETS THE PERSON THROUGH, and the
  // direction of that choice is the whole of it.
  //
  // The answer is one round trip away, so whichever way this falls there is a
  // brief moment before it settles. Holding here would show "Choose where this
  // cluster thinks" to EVERY user on EVERY page load, ahead of the page they
  // asked for -- a permanent tax on the common case, which after first run is
  // an already-configured cluster. Letting them through instead costs a
  // first-run user one flash of the console before the gate replaces it: once,
  // ever, for the people the gate is actually for.
  //
  // It is also the more honest reading. We have not asked the question yet, so
  // blocking on it asserts something we do not know.
  if (input.status === null) return "console";
  return "inference";
}

// canSkipInference is true in exactly one mode. Stated as its own function
// because it is the sort of condition that grows an "|| isDev" a year later,
// and a function with this name makes that edit visible.
export function canSkipInference(authEnabled: boolean): boolean {
  return !authEnabled;
}

export function useInferenceStatus(enabled: boolean): InferenceStatusState {
  const { query, status: connection } = useCluster();
  const [status, setStatus] = useState<InferenceStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!enabled || !query || connection !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    query
      .inferenceStatus({})
      .then((result: unknown) => {
        if (stale) return;
        setStatus(toInferenceStatus(materialize(result)));
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
  }, [enabled, query, connection, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return useMemo(() => ({ status, loading, error, reload }), [status, loading, error, reload]);
}
