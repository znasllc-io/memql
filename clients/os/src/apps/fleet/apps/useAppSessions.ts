import { useEffect, useMemo, useRef, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import { useReading, type Reading } from "../../../cluster/reading";
import {
  appSessionDetailFromRow,
  appSessionFromRow,
  sessionIsLive,
  sessionsNewestFirst,
  type AppSessionDetailRow,
  type AppSessionRow,
} from "../rows";

// Delegated app sessions: the caller's list, and one run's transcript
// (epic memql#5009).
//
// ===========================================================================
// NEITHER IS LIVE, AND THAT IS THE HONEST ANSWER
// ===========================================================================
// `v1:worker:appSession` carries no broadcast routing rule
// (component/node/routing.go names registration, routingPolicy and
// workbench:workspace and stops), so a subscription over it would render
// "Loading from the cluster" and then a list that never moves. The list reads
// once, says when it looked, and offers to look again -- `useReading`.
//
// THE TRANSCRIPT IS THE ONE EXCEPTION, and it POLLS rather than subscribes:
// the row grows while the run is going and stops when it ends, so the detail
// re-reads on a timer WHILE the status is starting or running and stops the
// moment it is not. A finished session re-read forever is load for a row that
// will never change again, and a surface that never settles.
//
// THE TWO READS PROJECT DIFFERENT SHAPES ON PURPOSE. `appSessionsForUser`
// answers `workerAppSessionCard`, which deliberately carries NO transcript --
// the largest field on the row by orders of magnitude. Only
// `appSessionById` (`workerAppSessionFull`) asks for it.
//
// THERE IS NO WRITE HERE AT ALL. Every writer of this concept is
// `@serverOnly` and absent from the SDK, so this surface is strictly a
// reading of somebody's run.

const POLL_INTERVAL_MS = 3000;

export interface AppSessionsState {
  sessions: AppSessionRow[];
  loading: boolean;
  error: string;
  readAt: Date | null;
  reread: () => void;
}

export function useAppSessions(): AppSessionsState {
  const connection = useOsConnection();

  const read = useMemo(() => {
    const query = connection?.query ?? null;
    if (query === null) return null;
    return async (signal: AbortSignal) => {
      // No ownerUserId argument: the query scopes on actor.userId at the
      // engine. An id the client chose would be a scope the server has to
      // distrust anyway.
      const result = await query.appSessionsForUser({}, { signal });
      return sessionsNewestFirst(
        result.rows().map(appSessionFromRow).filter((one) => one.id !== ""),
      );
    };
  }, [connection]);

  const reading: Reading<AppSessionRow[]> = useReading("fleet:appSessions", read);

  return {
    sessions: reading.value ?? [],
    loading: reading.state === "reading" || reading.state === "unread",
    error: reading.error,
    readAt: reading.at,
    reread: reading.reread,
  };
}

export interface AppSessionDetailState {
  session: AppSessionDetailRow | null;
  loading: boolean;
  error: string;
  /** When the most recent read landed. */
  readAt: Date | null;
  /** True while the view is re-reading a LIVE run, so the surface can say
   *  "refreshing" honestly rather than implying a subscription it has not
   *  got. False the instant the status turns terminal. */
  polling: boolean;
  reread: () => void;
}

export function useAppSessionDetail(sessionId: string): AppSessionDetailState {
  const connection = useOsConnection();
  const [session, setSession] = useState<AppSessionDetailRow | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState<Date | null>(null);
  const [polling, setPolling] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || sessionId === "") return;
    let live = true;

    const read = (first: boolean): void => {
      if (first) setLoading(true);
      void query
        .appSessionById({ sessionId })
        .then((result) => {
          if (!live) return;
          const row = result.rows()[0];
          if (row === undefined) {
            setSession(null);
            setPolling(false);
            setReadAt(new Date());
            return;
          }
          const detail = appSessionDetailFromRow(row);
          setSession(detail);
          setError("");
          setReadAt(new Date());
          // POLL ONLY WHILE THE RUN IS LIVE. Terminal is terminal: the
          // engine drops out-of-order and duplicate chunks, so an ended
          // transcript is final and a timer over it never settles.
          if (sessionIsLive(detail.status)) {
            setPolling(true);
            timer.current = setTimeout(() => read(false), POLL_INTERVAL_MS);
          } else {
            setPolling(false);
          }
        })
        .catch((err: unknown) => {
          if (!live) return;
          // The server's own sentence. A wrapper of ours would name this
          // surface instead of naming the refusal.
          setError(err instanceof Error ? err.message : String(err));
          setPolling(false);
        })
        .finally(() => {
          if (live && first) setLoading(false);
        });
    };

    read(true);

    return () => {
      live = false;
      if (timer.current !== null) clearTimeout(timer.current);
      timer.current = null;
    };
  }, [connection, sessionId, attempt]);

  return {
    session,
    loading,
    error,
    readAt,
    polling,
    reread: () => setAttempt((n) => n + 1),
  };
}
