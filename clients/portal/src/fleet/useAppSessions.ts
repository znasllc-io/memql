import { useCallback, useEffect, useRef, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

// Delegated app sessions: the caller's list, and one session's live
// transcript (memql#4363).
//
// # The list and the detail read DIFFERENT shapes, deliberately
//
// appSessionsForUser projects a CARD -- no transcript. The transcript is the
// largest field on the row by orders of magnitude, so a list that projected
// it would pull megabytes to render a handful of lines. The detail read is
// the only one that asks for it.
//
// # A live session polls; a finished one does not
//
// The transcript grows while the run is going, and stops when it ends. So the
// detail view polls only while the status is non-terminal and stops the
// moment it is not -- a finished session re-read on a timer is pure load for
// a row that will never change again.

const POLL_INTERVAL_MS = 3000;
const LIVE_STATUSES = new Set(["starting", "running"]);

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface AppSessionUsage {
  inputTokens: number;
  outputTokens: number;
  costUSD: number;
  known: boolean;
}

export interface AppSessionCard {
  id: string;
  app: string;
  kind: string;
  status: string;
  billing: string;
  planId: string;
  taskId: string;
  workerId: string;
  exitCode: number;
  usage: AppSessionUsage;
  startedAt: string;
  endedAt: string;
}

export interface AppSessionDetail extends AppSessionCard {
  workspace: string;
  prompt: string;
  transcript: string;
  transcriptBytes: number;
  transcriptTruncated: boolean;
  producedArtifactIds: string[];
  appSessionRef: string;
  mcpEndpoint: string;
  credentialExpiresAt: string;
  errorMessage: string;
  cancelReason: string;
}

export function isLive(status: string): boolean {
  return LIVE_STATUSES.has(status);
}

function numberField(row: Row, key: string): number {
  const raw = (row as Record<string, unknown>)[key];
  return typeof raw === "number" && Number.isFinite(raw) ? raw : 0;
}

function usageFrom(row: Row): AppSessionUsage {
  const raw = (row as Record<string, unknown>)["usage"];
  if (typeof raw !== "object" || raw === null) {
    return { inputTokens: 0, outputTokens: 0, costUSD: 0, known: false };
  }
  const usage = raw as Record<string, unknown>;
  const num = (key: string): number =>
    typeof usage[key] === "number" && Number.isFinite(usage[key] as number) ? (usage[key] as number) : 0;
  return {
    inputTokens: num("inputTokens"),
    outputTokens: num("outputTokens"),
    costUSD: num("costUSD"),
    known: usage["known"] === true,
  };
}

function stringList(row: Row, key: string): string[] {
  const raw = (row as Record<string, unknown>)[key];
  if (!Array.isArray(raw)) return [];
  return raw.filter((item): item is string => typeof item === "string" && item !== "");
}

function cardFrom(row: Row): AppSessionCard {
  return {
    id: rowString(row, "id"),
    app: rowString(row, "app"),
    kind: rowString(row, "kind"),
    status: rowString(row, "status"),
    billing: rowString(row, "billing") || "unknown",
    planId: rowString(row, "planId"),
    taskId: rowString(row, "taskId"),
    workerId: rowString(row, "workerId"),
    exitCode: numberField(row, "exitCode"),
    usage: usageFrom(row),
    startedAt: rowString(row, "startedAt"),
    endedAt: rowString(row, "endedAt"),
  };
}

function detailFrom(row: Row): AppSessionDetail {
  return {
    ...cardFrom(row),
    workspace: rowString(row, "workspace"),
    prompt: rowString(row, "prompt"),
    transcript: rowString(row, "transcript"),
    transcriptBytes: numberField(row, "transcriptBytes"),
    transcriptTruncated: (row as Record<string, unknown>)["transcriptTruncated"] === true,
    producedArtifactIds: stringList(row, "producedArtifactIds"),
    appSessionRef: rowString(row, "appSessionRef"),
    mcpEndpoint: rowString(row, "mcpEndpoint"),
    credentialExpiresAt: rowString(row, "credentialExpiresAt"),
    errorMessage: rowString(row, "errorMessage"),
    cancelReason: rowString(row, "cancelReason"),
  };
}

export interface AppSessionsState {
  sessions: AppSessionCard[];
  loading: boolean;
  error: string;
  reload: () => void;
}

export function useAppSessions(): AppSessionsState {
  const { query } = useCluster();
  const [sessions, setSessions] = useState<AppSessionCard[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    // No ownerUserId argument: the query scopes on actor.userId at the
    // engine. Passing an id the client chose would be a scope the server
    // then has to distrust anyway.
    void query
      .appSessionsForUser({})
      .then((res) => {
        if (live) setSessions(res.rows().map(cardFrom));
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { sessions, loading, error, reload };
}

export interface AppSessionDetailState {
  session: AppSessionDetail | null;
  loading: boolean;
  error: string;
  // polling is true while the view is re-reading a live session, so the
  // header can say "live" honestly rather than implying a subscription.
  polling: boolean;
}

export function useAppSessionDetail(sessionId: string): AppSessionDetailState {
  const { query } = useCluster();
  const [session, setSession] = useState<AppSessionDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [polling, setPolling] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (query === null || sessionId === "") return;
    let live = true;

    const read = (first: boolean): void => {
      if (first) setLoading(true);
      void query
        .appSessionById({ sessionId })
        .then((res) => {
          if (!live) return;
          const first = res.rows()[0];
          if (first === undefined) {
            setSession(null);
            setPolling(false);
            return;
          }
          const detail = detailFrom(first);
          setSession(detail);
          // Poll ONLY while the run is live. A finished session re-read on
          // a timer is pure load for a row that will never change again.
          if (isLive(detail.status)) {
            setPolling(true);
            timer.current = setTimeout(() => read(false), POLL_INTERVAL_MS);
          } else {
            setPolling(false);
          }
        })
        .catch((err: unknown) => {
          if (!live) return;
          setError(describe(err));
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
    };
  }, [query, sessionId]);

  return { session, loading, error, polling };
}
