import { useCallback, useEffect, useMemo, useState } from "react";

import { useCluster } from "../cluster/ClusterProvider";

// Data wiring for Settings -> AI providers (epic memql#4440).
//
// REQUEST/REPLY WITH AN EXPLICIT REFRESH, not a subscription -- the same
// deliberate downgrade useDataOrigins.ts documents, and for a sharper reason
// here: `providerAuthStatus` is a projection of THIS NODE's in-memory provider
// registry, not of rows anyone writes, so there is no graph event to subscribe
// to. A page that appeared live while showing a registry that stopped moving
// would invite an operator to trust a reading that was taken minutes ago,
// which on this page is the difference between "the key took" and "the key
// took on the replica you happened to reach".
//
// WHICH REPLICA ANSWERED IS NOT KNOWABLE FROM HERE, and the page says so
// rather than implying a fleet-wide reading. The front door routes each call
// independently, so two Refreshes can be answered by two nodes. That is why
// Apply broadcasts instead of relying on repeated reads.

export interface ProviderStatusRow {
  name: string;
  vendor: string;
  model: string;
  available: boolean;
  authSource: string;
  reason: string;
}

export interface ProvidersState {
  rows: ProviderStatusRow[];
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

// materialize absorbs both shapes a caller can hand back: the SDK's Result,
// and a plain array (what a component test constructs). Copied in spirit from
// useDataOrigins for the same reason it exists there -- a page test should not
// have to build an SDK Result to say what the server returned.
function materialize(result: unknown): RowBag[] {
  if (Array.isArray(result)) return result as RowBag[];
  const bag = result as { rows?: () => unknown } | null;
  if (bag && typeof bag.rows === "function") {
    const rows = bag.rows();
    if (Array.isArray(rows)) return rows as RowBag[];
  }
  return [];
}

export function toProviderRows(rows: readonly RowBag[]): ProviderStatusRow[] {
  return rows.map((row) => ({
    name: str(row, "name"),
    vendor: str(row, "vendor"),
    model: str(row, "model"),
    available: bool(row, "available"),
    authSource: str(row, "authSource"),
    reason: str(row, "reason"),
  }));
}

// summarize is what the page's headline says, and it exists as a pure function
// so the three states can be tested without rendering.
//
// THE KEYLESS STATE IS NOT AN ERROR STATE. "No AI providers are configured" is
// how a correctly-installed cluster starts, and the copy has to read as a next
// step rather than a fault -- an operator who reads a red banner on a fresh
// install concludes the install failed.
export function summarize(rows: readonly ProviderStatusRow[]): {
  tone: "unconfigured" | "partial" | "ready";
  headline: string;
} {
  const total = rows.length;
  const available = rows.filter((r) => r.available).length;
  if (total === 0 || available === 0) {
    return {
      tone: "unconfigured",
      headline: "No AI provider is configured yet, which is how a cluster is installed.",
    };
  }
  if (available < total) {
    return {
      tone: "partial",
      headline: `${available} of ${total} providers can be called.`,
    };
  }
  return { tone: "ready", headline: `All ${total} providers can be called.` };
}

export function useProviderStatus(enabled: boolean): ProvidersState {
  const { query, status } = useCluster();
  const [rows, setRows] = useState<ProviderStatusRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!enabled || !query || status !== "connected") return;
    let stale = false;
    setLoading(true);
    setError("");
    query
      .providerAuthStatus({})
      .then((result) => {
        if (stale) return;
        setRows(toProviderRows(materialize(result)));
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
  return useMemo(() => ({ rows, loading, error, reload }), [rows, loading, error, reload]);
}

// One in-flight write and what came of it. Shared by the four actions so the
// page can only ever have one of them running -- Apply while a key is still
// being sealed would reload a registry against a row that is not there yet.
export interface ActionState {
  busy: boolean;
  message: string;
  failed: boolean;
}

export const IDLE_ACTION: ActionState = { busy: false, message: "", failed: false };

export interface ProviderActions {
  state: ActionState;
  saveKey: (vendor: string, apiKey: string) => Promise<void>;
  saveFederation: (fields: Record<string, string>) => Promise<void>;
  verify: (provider: string) => Promise<void>;
  apply: () => Promise<void>;
}

// firstRow reads the single-row reply these actions return. An empty reply is
// reported as such rather than defaulted: an action whose result cannot be
// read has not been shown to have worked.
function firstRow(result: unknown): RowBag | null {
  const rows = materialize(result);
  return rows.length > 0 ? rows[0] ?? null : null;
}

export function useProviderActions(onChanged: () => void): ProviderActions {
  const { query } = useCluster();
  const [state, setState] = useState<ActionState>(IDLE_ACTION);

  const run = useCallback(
    async (work: () => Promise<string>): Promise<void> => {
      setState({ busy: true, message: "", failed: false });
      try {
        const message = await work();
        setState({ busy: false, message, failed: false });
        onChanged();
      } catch (err: unknown) {
        setState({
          busy: false,
          message: err instanceof Error ? err.message : String(err),
          failed: true,
        });
      }
    },
    [onChanged],
  );

  const saveKey = useCallback(
    async (vendor: string, apiKey: string) =>
      run(async () => {
        if (!query) throw new Error("not connected");
        const row = firstRow(await query.providerKeySet({ vendor, apiKey }));
        if (row === null) throw new Error("the cluster returned no result for the key it sealed");
        // The FINGERPRINT, never the key -- there is no read-back call, and
        // this is the only thing about the value the page ever renders.
        const fingerprint = str(row, "fingerprint");
        return `Sealed as ${str(row, "name")}${fingerprint === "" ? "" : ` (${fingerprint})`}. ${str(row, "message")}`;
      }),
    [query, run],
  );

  const saveFederation = useCallback(
    async (fields: Record<string, string>) =>
      run(async () => {
        if (!query) throw new Error("not connected");
        const row = firstRow(await query.providerFederationSet(fields));
        if (row === null) throw new Error("the cluster returned no result for the federation write");
        return str(row, "message");
      }),
    [query, run],
  );

  const verify = useCallback(
    async (provider: string) =>
      run(async () => {
        if (!query) throw new Error("not connected");
        const row = firstRow(await query.providerVerify({ provider }));
        if (row === null) throw new Error("the cluster returned no verification result");
        // A REFUSAL IS A RESULT, not a thrown error: the engine returns
        // verified=false with the vendor's own words, and rendering that as an
        // exception would blame the console for the vendor's answer.
        if (bool(row, "verified")) {
          return `${provider}: the vendor accepted this credential.`;
        }
        throw new Error(`${provider}: ${str(row, "reason") || "the vendor did not accept this credential."}`);
      }),
    [query, run],
  );

  const apply = useCallback(
    async () =>
      run(async () => {
        if (!query) throw new Error("not connected");
        const row = firstRow(await query.providersReload({}));
        if (row === null) throw new Error("the cluster returned no result for the reload");
        return (
          `Reloaded. The node that answered can call ${String(row["availableOnThisNode"] ?? "?")} ` +
          `of ${String(row["registered"] ?? "?")} providers, and every other node was told to re-resolve.`
        );
      }),
    [query, run],
  );

  return useMemo(
    () => ({ state, saveKey, saveFederation, verify, apply }),
    [state, saveKey, saveFederation, verify, apply],
  );
}
