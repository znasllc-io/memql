import { useCallback, useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { readNodeTokens, readPeople, readTokensForUser } from "./adminWire";

// The two credential populations Settings -> Tokens shows (epic memql#4984),
// and the fan-out one of them needs.
//
// A PERSONAL ACCESS TOKEN AND A NODE CREDENTIAL ARE NOT THE SAME KIND OF
// THING, and the section keeps them apart rather than merging them into one
// "credentials" list. A PAT is a person acting as themselves from a CLI; a
// node token is a process the cluster bootstrapped. They are revoked through
// two different ops, they fail differently, and an operator hunting a leaked
// key already knows which of the two they are looking for.

function str(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

export interface TokenRow {
  id: string;
  owner: string;
  label: string;
  active: boolean;
  lastUsedAt: string;
  createdAt: string;
  usableByAgents: boolean;
}

export interface NodeTokenRow {
  id: string;
  node: string;
  nodeType: string;
  active: boolean;
  mintedBy: string;
  expiresAt: string;
  lastConnectAt: string;
  createdAt: string;
}

/** Join one person's PAT rows to that person's display identity. */
export function toTokenRows(rows: readonly Row[], owner: string): TokenRow[] {
  const out: TokenRow[] = [];
  for (const row of rows) {
    const id = str(row, "id");
    if (id === "") continue;
    out.push({
      id,
      owner,
      label: str(row, "label") || "(no label)",
      active: row["active"] !== false,
      lastUsedAt: str(row, "lastUsedAt"),
      createdAt: str(row, "createdAt"),
      usableByAgents: row["usableByAgents"] === true,
    });
  }
  return out;
}

export function toNodeTokenRows(rows: readonly Row[]): NodeTokenRow[] {
  const out: NodeTokenRow[] = [];
  for (const row of rows) {
    const id = str(row, "id");
    if (id === "") continue;
    out.push({
      id,
      node: str(row, "nodeId") || "(unbound)",
      nodeType: str(row, "nodeType") || "unknown",
      active: row["active"] !== false,
      mintedBy: str(row, "mintedBy") || "the bootstrap path",
      expiresAt: str(row, "expiresAt"),
      lastConnectAt: str(row, "lastConnectAt"),
      createdAt: str(row, "createdAt"),
    });
  }
  return out;
}

/** How this person is named in the list, best available first. */
export function ownerNameOf(person: Row): string {
  return (
    str(person, "primaryEmail") || str(person, "displayName") || str(person, "id") || "(unknown)"
  );
}

// The section reads at most this many people's tokens in one pass. There is no
// cluster-wide PAT query to call instead (see adminWire.ts), so this is a
// fan-out, and a fan-out needs a ceiling: an unbounded one turns opening the
// section on a large cluster into thousands of stream requests.
export const MAX_PEOPLE_SCANNED = 200;
// How many of those run at once. The stream multiplexes, so this is politeness
// toward the node rather than a correctness bound.
const FAN_OUT_WIDTH = 8;

export async function readAllTokens(
  conn: NonNullable<ReturnType<typeof useOsConnection>>,
  people: readonly Row[],
  signal: AbortSignal,
): Promise<TokenRow[]> {
  const out: TokenRow[] = [];
  const scanned = people.slice(0, MAX_PEOPLE_SCANNED);

  for (let i = 0; i < scanned.length; i += FAN_OUT_WIDTH) {
    if (signal.aborted) break;
    const window = scanned.slice(i, i + FAN_OUT_WIDTH);
    const pages = await Promise.all(
      window.map(async (person) => {
        const userId = str(person, "id");
        if (userId === "") return [];
        return toTokenRows(await readTokensForUser(conn, userId, signal), ownerNameOf(person));
      }),
    );
    for (const page of pages) out.push(...page);
  }

  // Newest first. The fan-out returns per-person groups, which is the order the
  // requests happened to complete in and means nothing to a reader.
  out.sort((a, b) => (a.createdAt < b.createdAt ? 1 : a.createdAt > b.createdAt ? -1 : 0));
  return out;
}

export interface TokenFacts {
  tokens: TokenRow[];
  nodeTokens: NodeTokenRow[];
  loading: boolean;
  error: string;
  /** How many people the fan-out covered, and whether it stopped short. A
   *  surface that hides what it could not examine is worse than one that
   *  examined less and said so. */
  scanned: number;
  people: number;
  capped: boolean;
  fetchedAt: number | null;
  reload: () => void;
}

export function useTokenFacts(enabled: boolean): TokenFacts {
  const connection = useOsConnection();
  const [tokens, setTokens] = useState<TokenRow[]>([]);
  const [nodeTokens, setNodeTokens] = useState<NodeTokenRow[]>([]);
  const [people, setPeople] = useState(0);
  const [scanned, setScanned] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void (async () => {
      // The node credentials first: one call, and it is the half that answers
      // even when the fan-out below is capped.
      const nodes = await readNodeTokens(connection, controller.signal);
      const roster = await readPeople(connection, controller.signal);
      const pats = await readAllTokens(connection, roster, controller.signal);
      if (stale) return;
      setNodeTokens(toNodeTokenRows(nodes));
      setPeople(roster.length);
      setScanned(Math.min(roster.length, MAX_PEOPLE_SCANNED));
      setTokens(pats);
      setFetchedAt(Date.now());
    })()
      .catch((err: unknown) => {
        if (stale) return;
        // The engine's own sentence, verbatim. An admin who is told
        // "patIdentitiesForUser is owner-or-admin" has been told exactly what
        // happened; a paraphrase of ours would not improve on it.
        setTokens([]);
        setNodeTokens([]);
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

  return useMemo(
    () => ({
      tokens,
      nodeTokens,
      loading,
      error,
      scanned,
      people,
      capped: people > MAX_PEOPLE_SCANNED,
      fetchedAt,
      reload,
    }),
    [tokens, nodeTokens, loading, error, scanned, people, fetchedAt, reload],
  );
}
