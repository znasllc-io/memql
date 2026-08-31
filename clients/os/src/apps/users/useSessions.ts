import { useEffect, useState } from "react";

import { useOsConnection } from "../../live/connection";

// How many sessions a person currently has, for the detail panel.
//
// ===========================================================================
// WHY THIS READS sessionsForSubjectAdmin AND NOT authSessionsForSubject
// ===========================================================================
// They are two queries over the same rows, and the difference is what makes
// one of them safe to run from a browser.
//
// `authSessionsForSubject` filters on `subject` and NOTHING else -- no role
// gate -- and projects `authSessionFull`, which carries `tokenHash`,
// `refreshTokenHash` and `previousRefreshTokenHash`. It is a SERVER read: its
// one caller is the all-sessions revoke handler, which passes the caller's own
// JWT `sub`, so the argument is never attacker-chosen there.
//
// This panel passes an id the reader clicked, from a browser. So it uses
// `sessionsForSubjectAdmin` (memql#4734), which carries `requiresOwnerOrAdmin`
// as a top-level conjunct and projects `authSessionAdminSummary` -- no token
// digests at all. Rendering a COUNT never needed the keys the auth hot path
// looks rows up by.
//
// BEST-EFFORT BY CONTRACT. A refusal or a failure degrades to "--" and never
// fails the panel: the sessions count is the least important line on it, and
// a person's role and sign-in policy must still render on a cluster where
// this read does not answer.

/**
 * The word the count is qualified with, and it is not decoration.
 *
 * The read pages at 50. An account with more than fifty sessions in its
 * history would report fifty as a TOTAL, which is false; reporting it as
 * "recent" is true at every size. There is no time window here -- "recent"
 * names the page, not a duration.
 */
const COUNT_QUALIFIER = "recent";

export interface SessionsCount {
  /** What the Fact renders: a number, "--" when unknown, or "0". */
  label: string;
  /** Null while in flight or unavailable. */
  count: number | null;
}

interface SessionRow {
  revokedAt?: unknown;
  expiresAt?: unknown;
}

/**
 * Live sessions among the newest 50 rows.
 *
 * COUNTED CLIENT-SIDE, and the query deliberately returns revoked rows so it
 * can be: a reader has to be able to tell "signed in on three devices" from
 * "signed in once and revoked twice", and a query that pre-filtered would make
 * those two look identical.
 *
 * The label's qualifier is explained on COUNT_QUALIFIER.
 */
function countLive(rows: readonly SessionRow[], now: number): number {
  let live = 0;
  for (const row of rows) {
    const revoked = typeof row.revokedAt === "string" ? row.revokedAt.trim() : "";
    if (revoked !== "") continue;
    const expires = typeof row.expiresAt === "string" ? Date.parse(row.expiresAt) : NaN;
    // An UNPARSEABLE or absent expiry counts as live. The alternative is
    // dropping a session we could not date, which understates the answer --
    // and this number exists to tell somebody how many ways into an account
    // are currently open.
    if (Number.isFinite(expires) && expires <= now) continue;
    live += 1;
  }
  return live;
}

export function useSessionsCount(userId: string): SessionsCount {
  const connection = useOsConnection();
  const [count, setCount] = useState<number | null>(null);

  useEffect(() => {
    setCount(null);
    if (connection === null || userId === "") return;
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const result = await connection.query.sessionsForSubjectAdmin(
          { subject: userId },
          { signal: controller.signal },
        );
        if (!live) return;
        setCount(countLive(result.rows() as SessionRow[], Date.now()));
      } catch {
        // Refused, or unavailable. Either way the panel says "--" and carries
        // on; see the header note.
        if (live) setCount(null);
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, userId]);

  return {
    count,
    label: count === null ? "--" : `${count} ${COUNT_QUALIFIER}`,
  };
}

export { countLive as countLiveSessions };
