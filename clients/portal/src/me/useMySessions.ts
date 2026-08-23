import { useCallback, useEffect, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";

// The Sessions tab's read and its two revokes (memql#4319).
//
// # "This device" comes from session_id, never from a user agent
//
// MyAccessResult.session_id names the v1:identity:authSession row backing
// THIS connection, read by the server off the verified claims. The portal
// refuses to decode its own bearer, so that field is the only honest way to
// mark the row a person is sitting in -- and guessing by user agent is worse
// than it sounds: two tabs of one browser share a user agent and hold
// different sessions, so a wrong guess invites somebody to revoke the session
// they are currently using.
//
// Empty session_id is not an error. A PAT, an operator key and a service
// account are bearers with no session row to name, and the list simply marks
// nothing.
//
// # Revoking is confirmed, and the current row is called out
//
// Every revoke is destructive and none of them is undoable, so each confirms.
// The one that ends THIS session says so in its own words rather than sharing
// the generic sentence -- "you will be signed out here" is the whole reason a
// person would hesitate.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface MeSession {
  id: string;
  // The recorded User-Agent, shortened. Not parsed into "Chrome on macOS":
  // the parse is a guess, and a person recognising their own device does not
  // need the browser named -- they need to tell two rows apart. Identity's
  // own /me/devices makes the same call, in sessionLabel().
  device: string;
  source: string;
  signedIn: string;
  lastActive: string;
  thisDevice: boolean;
}

export interface MySessionsState {
  sessions: MeSession[];
  loading: boolean;
  error: string;
  reload: () => void;
  busyId: string;
  actionError: string;
  // Revoke ONE row. Resolves to true when the row revoked was this
  // connection's, so the caller can sign out rather than re-reading a list it
  // can no longer read.
  revoke: (sessionId: string) => Promise<boolean>;
  revokeAll: () => Promise<boolean>;
}

// SOURCE, in the words a person would use. The stored values name the
// mechanism that minted the row, which is an implementation fact.
const SOURCE_LABELS: Record<string, string> = {
  oidc_cookie: "Browser",
  bff_exchange: "Application",
  device_code: "Device code",
};

function sessionFromRow(row: Row, currentSessionId: string): MeSession {
  const id = rowString(row, "id");
  const ua = rowString(row, "clientLabel").trim();
  const source = rowString(row, "source").trim();
  return {
    id,
    device: ua === "" ? "Unknown client" : ua.length > 72 ? `${ua.slice(0, 72)}...` : ua,
    source: SOURCE_LABELS[source] ?? (source === "" ? "Unknown" : source),
    signedIn: rowString(row, "firstAuthenticatedAt") || rowString(row, "createdAt"),
    lastActive: rowString(row, "lastActivityAt") || rowString(row, "lastRefreshedAt"),
    thisDevice: currentSessionId !== "" && id === currentSessionId,
  };
}

export function useMySessions(): MySessionsState {
  const { query, clients } = useCluster();
  const { access } = useMyAccess();
  const currentSessionId = access?.sessionId ?? "";

  const [sessions, setSessions] = useState<MeSession[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [busyId, setBusyId] = useState("");
  const [actionError, setActionError] = useState("");

  useEffect(() => {
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    void query
      .authSessionsForSelf({})
      .then((res) => {
        if (!live) return;
        const rows = res.rows().map((row) => sessionFromRow(row, currentSessionId));
        // This device first, then newest. The row a person is least likely to
        // want to revoke is the one they most need to recognise before they
        // revoke anything else -- the ordering identity's own panel uses, for
        // the same reason.
        rows.sort((a, b) => {
          if (a.thisDevice !== b.thisDevice) return a.thisDevice ? -1 : 1;
          return b.signedIn.localeCompare(a.signedIn);
        });
        setSessions(rows);
      })
      .catch((err: unknown) => {
        // A LISTING ERROR IS NOT AN EMPTY LIST. The tab renders the failure;
        // an empty table would read as "no other device can reach your
        // account", which is the one wrong answer that reassures.
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch, currentSessionId]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  const revoke = useCallback(
    async (sessionId: string): Promise<boolean> => {
      if (clients === null || sessionId === "") return false;
      setBusyId(sessionId);
      setActionError("");
      try {
        const res = await clients.revokeSession(sessionId);
        if (!res.success) {
          setActionError(res.errorMessage || "That session could not be ended.");
          setEpoch((n) => n + 1);
          return false;
        }
        setEpoch((n) => n + 1);
        return res.wasCurrent;
      } catch (err: unknown) {
        setActionError(describe(err));
        return false;
      } finally {
        setBusyId("");
      }
    },
    [clients],
  );

  const revokeAll = useCallback(async (): Promise<boolean> => {
    if (clients === null) return false;
    setBusyId("all");
    setActionError("");
    try {
      const res = await clients.revokeAllSessions();
      if (!res.success) {
        setActionError(res.errorMessage || "Those sessions could not be ended.");
        setEpoch((n) => n + 1);
        return false;
      }
      // revokeAllSessions ends EVERY session the caller owns, this one
      // included -- it is "sign out everywhere", not "everywhere else". The
      // engine has no everywhere-else call, and inventing one client-side by
      // looping revokeSession over the other rows would be a different
      // operation wearing this one's name: partial failures would leave a
      // person believing they had signed out of a device they had not.
      return true;
    } catch (err: unknown) {
      setActionError(describe(err));
      return false;
    } finally {
      setBusyId("");
    }
  }, [clients]);

  return { sessions, loading, error, reload, busyId, actionError, revoke, revokeAll };
}
