import { useCallback, useEffect, useState } from "react";
import {
  type Module,
  type ModuleDetail,
  type ModulesInventory,
  type SetPackEnabledOutcome,
} from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Data wiring for the Modules surface (memql#4191).
//
// The inventory is a REQUEST/REPLY read, not a subscription: module state
// changes when an operator flips a pack or a deploy rolls a node, both rare,
// and the engine exposes no stream for it -- so the hooks fetch on mount and
// on an explicit reload, and the page carries a Refresh action rather than
// pretending to be live. (Pack ENABLEMENT is graph state and could be
// subscribed; the registry rows it joins against are not, and a half-live
// surface would mislead more than a dated one with an honest Refresh.)

export interface ModulesState {
  inventory: ModulesInventory | null;
  loading: boolean;
  error: string;
  reload: () => void;
}

export function useModulesInventory(enabled: boolean): ModulesState {
  const { clients, status } = useCluster();
  const [inventory, setInventory] = useState<ModulesInventory | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  const client = clients?.modules ?? null;

  useEffect(() => {
    if (!enabled || !client || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    client
      .listModules()
      .then((result) => {
        if (!stale) setInventory(result);
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
  }, [enabled, client, status, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { inventory, loading, error, reload };
}

export interface ModuleDetailState {
  detail: ModuleDetail | null;
  loading: boolean;
  error: string;
  reload: () => void;
}

export function useModuleDetail(kind: string, name: string, enabled: boolean): ModuleDetailState {
  const { clients, status } = useCluster();
  const [detail, setDetail] = useState<ModuleDetail | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  const client = clients?.modules ?? null;

  useEffect(() => {
    if (!enabled || !client || status !== "connected" || kind === "" || name === "") return;
    let stale = false;
    setLoading(true);
    setError("");
    client
      .getModuleDetail(kind, name)
      .then((result) => {
        if (!stale) setDetail(result);
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
  }, [enabled, client, status, kind, name, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { detail, loading, error, reload };
}

export interface PackFlipState {
  busy: boolean;
  error: string;
  outcome: SetPackEnabledOutcome | null;
  flip: (packDomain: string, enabled: boolean, reason: string, onDone: () => void) => void;
}

export function useSetPackEnabled(): PackFlipState {
  const { clients } = useCluster();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [outcome, setOutcome] = useState<SetPackEnabledOutcome | null>(null);

  const flip = useCallback(
    (packDomain: string, enabled: boolean, reason: string, onDone: () => void) => {
      if (!clients) {
        setError("not connected");
        return;
      }
      setBusy(true);
      setError("");
      setOutcome(null);
      clients.modules
        .setPackEnabled(packDomain, enabled, reason)
        .then((result) => {
          setOutcome(result);
          onDone();
        })
        .catch((err: unknown) => {
          setError(err instanceof Error ? err.message : String(err));
        })
        .finally(() => setBusy(false));
    },
    [clients],
  );

  return { busy, error, outcome, flip };
}

// Grouping + presentation facts the browser and detail share.

export const KIND_ORDER = ["pack", "integration", "node-type", "component"] as const;

export const KIND_LABELS: Record<string, string> = {
  pack: "Packs",
  integration: "Integrations",
  "node-type": "Node types",
  component: "Components",
};

export const KIND_BLURBS: Record<string, string> = {
  pack: "Product features with a per-instance switch. A flip takes effect as each node restarts.",
  integration: "Connections to other systems. Their state is derived from configuration, not stored.",
  "node-type": "Deployment units of the mesh. Their switch is replica scale.",
  component: "Engine internals, reported for their environment surface. Not switchable.",
};

export function groupByKind(modules: readonly Module[]): Array<{ kind: string; rows: Module[] }> {
  const byKind = new Map<string, Module[]>();
  for (const row of modules) {
    const list = byKind.get(row.kind) ?? [];
    list.push(row);
    byKind.set(row.kind, list);
  }
  const ordered: Array<{ kind: string; rows: Module[] }> = [];
  for (const kind of KIND_ORDER) {
    const rows = byKind.get(kind);
    if (rows) ordered.push({ kind, rows: rows.sort((a, b) => a.name.localeCompare(b.name)) });
    byKind.delete(kind);
  }
  // An unknown kind is an engine newer than this portal; show it rather than
  // hide it -- the row's own strings carry the meaning.
  for (const [kind, rows] of byKind) {
    ordered.push({ kind, rows: rows.sort((a, b) => a.name.localeCompare(b.name)) });
  }
  return ordered;
}

// State -> badge tone. Warnings are the states an operator probably came
// here to find; neutral covers deliberate offs.
export function stateTone(state: string): "ok" | "warn" | "danger" | "neutral" {
  switch (state) {
    case "enabled":
    case "active":
    case "built_in":
    case "running":
      return "ok";
    case "credential_gated":
    case "opted_out":
      return "warn";
    case "disabled":
    case "compiled_out":
    case "scaled_to_zero":
    case "not_deployed":
      return "neutral";
    default:
      return "neutral";
  }
}
