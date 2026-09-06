import { useCallback, useEffect, useMemo, useState } from "react";

import { useOsConnection } from "../../live/connection";

// Data wiring for Settings -> AI providers (epic memql#4984; the surface it
// replaces was the portal's, epic memql#4440).
//
// REQUEST/REPLY WITH AN EXPLICIT REFRESH, not a subscription, and the reason
// is sharper here than on the sections beside it: `providerAuthStatus` is a
// projection of THIS NODE's in-memory provider registry, not of rows anyone
// writes, so there is no graph event to subscribe to. A panel that appeared
// live while showing a registry that stopped moving would invite an operator
// to trust a reading taken minutes ago -- which on this surface is the
// difference between "the key took" and "the key took on the replica you
// happened to reach".
//
// WHICH REPLICA ANSWERED IS NOT KNOWABLE FROM HERE, and the section says so
// rather than implying a fleet-wide reading. The front door routes each call
// independently, so two Refreshes can be answered by two nodes. That is why
// Apply broadcasts rather than relying on repeated reads.

export interface ProviderStatusRow {
  name: string;
  vendor: string;
  model: string;
  available: boolean;
  authSource: string;
  reason: string;
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

/**
 * Absorb both shapes a caller can hand back: the SDK's Result, and a plain
 * array (what a section test constructs). A test should not have to build an
 * SDK Result to say what the server returned.
 */
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

export type ProviderTone = "unconfigured" | "partial" | "ready";

/**
 * What the section's opening line says, as a pure function so the three states
 * can be tested without rendering.
 *
 * THE KEYLESS STATE IS NOT AN ERROR STATE. "No AI provider is configured" is
 * how a correctly-installed cluster starts -- installing spends no inference
 * and asks for no key -- so the copy has to read as a next step rather than a
 * fault. An operator who meets a red banner on a fresh install concludes the
 * install failed.
 */
export function summarize(rows: readonly ProviderStatusRow[]): {
  tone: ProviderTone;
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
    return { tone: "partial", headline: `${available} of ${total} providers can be called.` };
  }
  return { tone: "ready", headline: `All ${total} providers can be called.` };
}

/** Vendor id to the name a person would use for it. */
export const VENDOR_LABELS: Record<string, string> = {
  anthropic: "Anthropic",
  openai: "OpenAI",
};

export function vendorLabel(vendor: string): string {
  return VENDOR_LABELS[vendor.toLowerCase()] ?? vendor;
}

/**
 * Where the tier names become sentences.
 *
 * The distinction that matters most is env versus the two row tiers: a key in
 * the pod's environment is the one source a save from here cannot change, so
 * an operator who saves a key and sees no change has to be TOLD that rather
 * than left to work it out.
 */
const AUTH_SOURCE_COPY: Record<string, string> = {
  federation: "workload identity, no key at rest",
  globalSecret: "a sealed row in this cluster",
  globalVariable: "a plaintext row in this cluster",
  env: "this pod's environment -- a saved key will not override it",
  unresolved: "nothing configured",
};

export function sourceCopy(source: string): string {
  return AUTH_SOURCE_COPY[source] ?? source;
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

export interface ProvidersState {
  rows: ProviderStatusRow[];
  loading: boolean;
  error: string;
  /** When the answer on screen was taken, or null before the first one. */
  fetchedAt: number | null;
  reload: () => void;
}

export function useProviderRegistry(enabled: boolean): ProvidersState {
  const connection = useOsConnection();
  const [rows, setRows] = useState<ProviderStatusRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  // An epoch counter, not a cache invalidation protocol: "what does a node say
  // right now" has no cache to invalidate.
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void connection.query
      .providerAuthStatus({}, { signal: controller.signal })
      .then((result) => {
        if (stale) return;
        setRows(toProviderRows(materialize(result)));
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (stale) return;
        // A server-side refusal arrives as a rejected promise carrying the
        // engine's own words. Rendered in-surface, never rewritten -- an admin
        // reading "providerAuthStatus is owner-only" has been told exactly what
        // happened, which no paraphrase of ours would improve on.
        setRows([]);
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled, epoch]);

  return { rows, loading, error, fetchedAt, reload };
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

export interface ProviderActionState {
  busy: boolean;
  message: string;
  failed: boolean;
}

export const IDLE_PROVIDER_ACTION: ProviderActionState = {
  busy: false,
  message: "",
  failed: false,
};

export interface ProviderActions {
  state: ProviderActionState;
  saveKey: (vendor: string, apiKey: string) => Promise<void>;
  saveFederation: (fields: Record<string, string>) => Promise<void>;
  verify: (provider: string) => Promise<void>;
  apply: () => Promise<void>;
}

/** Read the single-row reply these actions return. An empty reply is reported
 *  as such rather than defaulted: an action whose result cannot be read has
 *  not been shown to have worked. */
function firstRow(result: unknown): RowBag | null {
  const rows = materialize(result);
  return rows.length > 0 ? (rows[0] ?? null) : null;
}

export function useProviderActions(onChanged: () => void): ProviderActions {
  const connection = useOsConnection();
  const [state, setState] = useState<ProviderActionState>(IDLE_PROVIDER_ACTION);

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
        if (connection === null) throw new Error("Not connected to the cluster.");
        const row = firstRow(await connection.query.providerKeySet({ vendor, apiKey }));
        if (row === null) throw new Error("the cluster returned no result for the key it sealed");
        // The FINGERPRINT, never the key. There is no read-back call, and this
        // is the only thing about the value this surface ever renders.
        const fingerprint = str(row, "fingerprint");
        const name = str(row, "name");
        return `Sealed as ${name}${fingerprint === "" ? "" : ` (${fingerprint})`}. ${str(row, "message")}`.trim();
      }),
    [connection, run],
  );

  const saveFederation = useCallback(
    async (fields: Record<string, string>) =>
      run(async () => {
        if (connection === null) throw new Error("Not connected to the cluster.");
        const row = firstRow(await connection.query.providerFederationSet(fields));
        if (row === null) throw new Error("the cluster returned no result for the federation write");
        return str(row, "message");
      }),
    [connection, run],
  );

  const verify = useCallback(
    async (provider: string) =>
      run(async () => {
        if (connection === null) throw new Error("Not connected to the cluster.");
        const row = firstRow(await connection.query.providerVerify({ provider }));
        if (row === null) throw new Error("the cluster returned no verification result");
        // A REFUSAL IS A RESULT, not a thrown error: the engine returns
        // verified=false with the vendor's own words, and rendering that as an
        // exception would blame this console for the vendor's answer.
        if (bool(row, "verified")) return `${provider}: the vendor accepted this credential.`;
        throw new Error(
          `${provider}: ${str(row, "reason") || "the vendor did not accept this credential."}`,
        );
      }),
    [connection, run],
  );

  const apply = useCallback(
    async () =>
      run(async () => {
        if (connection === null) throw new Error("Not connected to the cluster.");
        const row = firstRow(await connection.query.providersReload({}));
        if (row === null) throw new Error("the cluster returned no result for the reload");
        return (
          `Reloaded. The node that answered can call ${String(row["availableOnThisNode"] ?? "?")} ` +
          `of ${String(row["registered"] ?? "?")} providers, and every other node was told to re-resolve.`
        );
      }),
    [connection, run],
  );

  return useMemo(
    () => ({ state, saveKey, saveFederation, verify, apply }),
    [state, saveKey, saveFederation, verify, apply],
  );
}
