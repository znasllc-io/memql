import { useCallback, useMemo, useState } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useSessionIfPresent } from "../../../chrome/access";
import { useLiveCollection } from "../../../live/useLiveCollection";

// One machine's app sign-ins: a LIVE feed, because the row is the only way to hear
// what the machine said (component/memql/fleet_app_signin.go), and the acts.
//
// Nothing is written through locally. The row arrives on the feed from the cluster,
// and an optimistic one would paint a sign-in the machine may refuse a moment later.

export const APP_SIGN_IN_CONCEPT = "v1:worker:appSignIn";

export interface AppSignIn {
  id: string;
  workerId: string;
  app: string;
  /** requested | waiting | succeeded | failed | cancelled. */
  status: string;
  /** The page to open. Empty unless it is an https address the machine reported. */
  url: string;
  /** What to enter on that page, for a device login. */
  userCode: string;
  /** The page shows a code to paste back here. */
  needsCode: boolean;
  errorMessage: string;
  requestedAt: string;
  expiresAt: string;
  endedAt: string;
}

/** The page's address, if it is one to send a person to. A machine reports this, and a
 *  link is the one place on this surface a reported string becomes something to click. */
function safeSignInUrl(raw: string): string {
  try {
    return new URL(raw).protocol === "https:" ? raw : "";
  } catch {
    return "";
  }
}

export function appSignInFromRow(raw: Row): AppSignIn {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    workerId: rowString(row, "workerId"),
    app: rowString(row, "app"),
    status: rowString(row, "status"),
    url: safeSignInUrl(rowString(row, "url")),
    userCode: rowString(row, "userCode"),
    needsCode: row["needsCode"] === true,
    errorMessage: rowString(row, "errorMessage"),
    requestedAt: rowString(row, "requestedAt"),
    expiresAt: rowString(row, "expiresAt"),
    endedAt: rowString(row, "endedAt"),
  };
}

/** Still in progress. A past expiresAt is over whatever the status says: nothing sweeps
 *  these rows, because a replica dying must not leave a person watching a spinner. */
export function appSignInIsLive(signIn: AppSignIn, now: Date): boolean {
  if (signIn.status !== "requested" && signIn.status !== "waiting") return false;
  const expires = Date.parse(signIn.expiresAt);
  return Number.isNaN(expires) || expires > now.getTime();
}

export interface AppSignInsHandle {
  /** The newest sign-in to this app on this machine, if any. */
  latest: (app: string) => AppSignIn | null;
  /** Each resolves to "" on success, or the refusal to show. */
  start: (app: string) => Promise<string>;
  submitCode: (signInId: string, code: string) => Promise<string>;
  cancel: (signInId: string) => Promise<string>;
  /** The act in flight, so one press is one call. */
  pending: boolean;
}

function refusalFrom(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  return raw.trim() === "" ? "The cluster refused and said nothing about why." : raw.trim();
}

/** workerId empty means no feed: only a machine's owner has one to read. */
export function useAppSignIns(workerId: string): AppSignInsHandle {
  const connection = useOsConnection();
  const userId = useSessionIfPresent()?.access?.userId ?? "";
  const [pending, setPending] = useState(false);

  const { snapshot } = useLiveCollection<Row>(
    !workerId || !userId ? null : `fleet:app-sign-ins:${userId}:${workerId}`,
    (conn) => ({
      concept: APP_SIGN_IN_CONCEPT,
      seed: async (_cursor, signal) => {
        const result = await conn.query.appSignInsForWorker({ workerId }, { signal });
        return { rows: result.rows(), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, APP_SIGN_IN_CONCEPT, rowId, { signal });
        return (row as Row) ?? null;
      },
      inScope: (row) => appSignInFromRow(row).workerId === workerId && rowString(flatten(row), "ownerUserId") === userId,
      paged: false,
    }),
  );

  const signIns = useMemo(
    () =>
      snapshot.rows
        .map(appSignInFromRow)
        .filter((s) => s.id !== "" && s.workerId === workerId)
        .sort((a, b) => (a.requestedAt < b.requestedAt ? 1 : a.requestedAt > b.requestedAt ? -1 : 0)),
    [snapshot, workerId],
  );

  const latest = useCallback((app: string) => signIns.find((s) => s.app === app) ?? null, [signIns]);

  const act = useCallback(
    async (call: () => Promise<unknown>): Promise<string> => {
      if (connection === null) return "This needs a live connection to the cluster.";
      setPending(true);
      try {
        await call();
        return "";
      } catch (err: unknown) {
        return refusalFrom(err);
      } finally {
        setPending(false);
      }
    },
    [connection],
  );

  const start = useCallback(
    (app: string) => act(() => connection!.query.fleetAppSignIn({ registrationId: workerId, app: app as "claude-code" | "codex" })),
    [act, connection, workerId],
  );
  const submitCode = useCallback(
    (signInId: string, code: string) => act(() => connection!.query.fleetAppSignInCode({ signInId, code })),
    [act, connection],
  );
  const cancel = useCallback(
    (signInId: string) => act(() => connection!.query.fleetAppSignInCancel({ signInId })),
    [act, connection],
  );

  return { latest, start, submitCode, cancel, pending };
}
