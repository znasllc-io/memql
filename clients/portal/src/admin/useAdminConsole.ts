import { useCallback, useEffect, useMemo, useState } from "react";
import type { QueryClient, Role, Row } from "@znasllc-io/memql-sdk-core/client";

import { useAuth } from "../auth/AuthProvider";
import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import { tokenRows, type TokenRow } from "./rows";
import {
  fetchSigningKeys,
  readAuditEvents,
  readClusterSettings,
  readPeople,
  readTokensForUser,
  type SigningKey,
} from "./wire";

// The admin console's state, one hook per question the screens ask.
//
// ===========================================================================
// THE ROLE READING IS A COURTESY. THE GATE IS THE CLUSTER'S.
// ===========================================================================
// `canAdminister` below decides what this console OFFERS. It decides nothing
// about what the cluster ANSWERS. Every read issues the same named query
// whatever this file believes, and every one of those queries carries
// `requiresOwnerOrAdmin` as a top-level conjunct evaluated server-side against
// the auth envelope -- so a reader who reaches these screens with the flag
// forced true still gets an empty result set from the engine, and a revoke
// still comes back refused.
//
// It is here for the reason src/deploy/useDeployConsole.ts states at length:
// showing an operator a table that will always be empty, and a button that
// will always fail, wastes their time and teaches them nothing about who can.
// Saying "your role is reader" is the honest interface.
const CAN_ADMINISTER: readonly Role[] = ["owner", "admin"];

export interface AdminAccess {
  role: Role;
  canAdminister: boolean;
  // False until the connection is up. Distinct from "you may not": a console
  // that says "reader" while still handshaking has told the operator
  // something false about themselves.
  resolved: boolean;
}

export function useAdminAccess(): AdminAccess {
  const { access, loading } = useMyAccess();
  const role: Role = access?.clusterRole ?? "";
  return {
    role,
    canAdminister: CAN_ADMINISTER.includes(role),
    resolved: !loading && access !== null,
  };
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// A read that runs when the connection is up and the caller may administer,
// and reports the three states a page has to tell apart: loading, failed, and
// "answered, and the answer is nothing".
export interface ReadState<T> {
  data: T;
  loading: boolean;
  error: string;
  reload: () => void;
}

function useGatedRead<T>(
  empty: T,
  read: (query: QueryClient, signal: AbortSignal) => Promise<T>,
  enabled: boolean,
  // Anything the read's identity depends on beyond the connection.
  key: string,
): ReadState<T> {
  const { query } = useCluster();
  const [data, setData] = useState<T>(empty);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || !enabled) {
      // Cleared rather than kept. A table left standing under a dead
      // connection reads as current, which is the one thing an operations
      // console must never imply.
      setData(empty);
      setLoading(false);
      setError("");
      return;
    }
    const controller = new AbortController();
    let live = true;
    setLoading(true);
    setError("");

    void read(query, controller.signal)
      .then((next) => {
        if (live) setData(next);
      })
      .catch((err: unknown) => {
        if (!live || controller.signal.aborted) return;
        setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
      controller.abort();
    };
    // `read` and `empty` are rebuilt every render by every caller, so they are
    // deliberately not dependencies -- `key` is what identifies the read, and
    // it is a string.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query, enabled, key, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { data, loading, error, reload };
}

const NO_ROWS: Row[] = [];

// ---------------------------------------------------------------------------
// People
// ---------------------------------------------------------------------------

export function usePeople(enabled: boolean): ReadState<Row[]> {
  return useGatedRead(NO_ROWS, (query, signal) => readPeople(query, signal), enabled, "people");
}

// ---------------------------------------------------------------------------
// The audit trail
// ---------------------------------------------------------------------------

// Every category the identity service writes under, in the order the audit
// page offers them. Mirrors identity.AuditCategory* -- a fixed vocabulary, so
// a select is honest where a free-text box would invite a typo that silently
// returns nothing.
export const AUDIT_CATEGORIES: readonly string[] = [
  "auth",
  "identity",
  "authorization",
  "configuration",
  "admin",
  "data",
];

export function useAuditTrail(enabled: boolean, category: string): ReadState<Row[]> {
  return useGatedRead(
    NO_ROWS,
    (query, signal) => readAuditEvents(query, category, signal),
    enabled,
    `audit:${category}`,
  );
}

// ---------------------------------------------------------------------------
// Cluster settings
// ---------------------------------------------------------------------------

export function useClusterSettings(enabled: boolean): ReadState<Row | null> {
  return useGatedRead<Row | null>(
    null,
    (query, signal) => readClusterSettings(query, signal),
    enabled,
    "settings",
  );
}

// ---------------------------------------------------------------------------
// Signing keys
// ---------------------------------------------------------------------------

export interface SigningKeyState {
  keys: SigningKey[];
  loading: boolean;
  error: string;
  // The identity origin the feed was read from, so the page can name it.
  origin: string;
  reload: () => void;
}

const NO_KEYS: SigningKey[] = [];

// The published key set. Read straight off the public feed rather than the
// stream -- see fetchSigningKeys for why that is the correct source and not a
// shortcut. Not gated on role for the same reason: the document is public, and
// pretending otherwise here would be theatre.
export function useSigningKeys(fetchImpl?: typeof globalThis.fetch): SigningKeyState {
  const { config } = useAuth();
  const origin = config.identityUrl;
  const [keys, setKeys] = useState<SigningKey[]>(NO_KEYS);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (origin === "") {
      setKeys(NO_KEYS);
      setLoading(false);
      setError("");
      return;
    }
    const controller = new AbortController();
    let live = true;
    setLoading(true);
    setError("");

    void fetchSigningKeys(origin, fetchImpl ?? globalThis.fetch, controller.signal)
      .then((next) => {
        if (live) setKeys(next);
      })
      .catch((err: unknown) => {
        if (!live || controller.signal.aborted) return;
        setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
      controller.abort();
    };
  }, [origin, fetchImpl, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { keys, loading, error, origin, reload };
}

// lastRotation picks the most recent successful key rotation out of a
// configuration-category audit read.
//
// This is what replaces the server-rendered console's "rotated 5d ago": that
// line came from the KeyManager's in-process createdAt, which no client can
// see. The audit event is the better answer anyway -- it is the exact instant,
// it says whether a human or the scheduler did it, and it is a row the
// operator can open. Empty when no rotation is in the window the query pages.
export interface Rotation {
  at: string;
  by: string;
}

export function lastRotation(events: readonly Row[]): Rotation | null {
  for (const event of events) {
    if (event["action"] !== "jwks_rotated") continue;
    if (event["outcome"] !== "success") continue;
    const at = typeof event["occurredAt"] === "string" ? event["occurredAt"] : "";
    const by = typeof event["actorEmail"] === "string" ? event["actorEmail"] : "";
    return { at, by: by === "" ? "the rotation schedule" : by };
  }
  return null;
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

export interface TokenConsoleState {
  tokens: TokenRow[];
  loading: boolean;
  error: string;
  // The people the fan-out covered, and whether it had to stop short.
  scanned: number;
  capped: boolean;
  reload: () => void;
}

// The console reads at most this many people's tokens in one pass. There is no
// cluster-wide PAT query to call instead (see wire.ts), so the page is a
// fan-out, and a fan-out needs a ceiling: an unbounded one turns opening a tab
// on a large cluster into thousands of stream requests.
const MAX_PEOPLE_SCANNED = 200;
// How many of those run at once. The stream multiplexes, so this is politeness
// toward the node rather than a correctness bound.
const FAN_OUT_WIDTH = 8;

async function readAllTokens(
  query: QueryClient,
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
        const userId = typeof person["id"] === "string" ? person["id"] : "";
        if (userId === "") return [];
        const owner =
          (typeof person["primaryEmail"] === "string" ? person["primaryEmail"] : "") ||
          (typeof person["displayName"] === "string" ? person["displayName"] : "") ||
          userId;
        return tokenRows(await readTokensForUser(query, userId, signal), owner);
      }),
    );
    for (const page of pages) out.push(...page);
  }

  // Newest first. The fan-out returns per-person groups, which is the order
  // the requests happened to complete in and means nothing to a reader.
  out.sort((a, b) => (a.createdAt < b.createdAt ? 1 : a.createdAt > b.createdAt ? -1 : 0));
  return out;
}

export function useTokenConsole(enabled: boolean): TokenConsoleState {
  const { query } = useCluster();
  const people = usePeople(enabled);
  const [tokens, setTokens] = useState<TokenRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  const peopleRows = people.data;

  useEffect(() => {
    if (query === null || !enabled || peopleRows.length === 0) {
      setTokens([]);
      setLoading(false);
      setError("");
      return;
    }
    const controller = new AbortController();
    let live = true;
    setLoading(true);
    setError("");

    void readAllTokens(query, peopleRows, controller.signal)
      .then((next) => {
        if (live) setTokens(next);
      })
      .catch((err: unknown) => {
        if (!live || controller.signal.aborted) return;
        setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
      controller.abort();
    };
  }, [query, enabled, peopleRows, epoch]);

  const reload = useCallback(() => {
    people.reload();
    setEpoch((n) => n + 1);
  }, [people]);

  const capped = peopleRows.length > MAX_PEOPLE_SCANNED;
  const scanned = useMemo(
    () => Math.min(peopleRows.length, MAX_PEOPLE_SCANNED),
    [peopleRows.length],
  );

  return {
    tokens,
    loading: people.loading || loading,
    error: people.error || error,
    scanned,
    capped,
    reload,
  };
}
