import { createContext, createElement, useContext, type ReactNode } from "react";
import type { LiveState, Row } from "@znasllc-io/memql-sdk-core/client";
import { useSessionIfPresent } from "../../../chrome/access";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { useLiveView, type LiveView } from "../../../live/liveView";

import {
  appSessionDetailFromRow,
  appSessionFromRow,
  sessionsNewestFirst,
  type AppSessionDetailRow,
  type AppSessionRow,
} from "../rows";

// Created and updated app-session events broadcast from the worker's agent
// replica to the browser's BFF (component/node/routing.go). One retained feed
// serves Equipment's short list and Activity's full list. The named reads and
// event scope both stay on the signed-in user's sessions, including for owners.
export const APP_SESSION_CONCEPT = "v1:worker:appSession";

export interface AppSessionsState {
  source: LiveView<AppSessionRow> | null;
  sessions: AppSessionRow[];
  state: LiveState;
  loading: boolean;
  error: string;
  reread: () => void;
}

const AppSessionsContext = createContext<AppSessionsState | null>(null);

function useAppSessionsFeed(enabled = true): AppSessionsState {
  const userId = useSessionIfPresent()?.access?.userId ?? "";
  const feed = useLiveCollection<Row>(enabled && userId ? `fleet:appSessions:${userId}` : null, connection => ({
    concept: APP_SESSION_CONCEPT,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.appSessionsForUser({}, { signal })).rows(), nextCursor: "" }),
    reread: async (sessionId, signal) => (await connection.query.appSessionById({ sessionId }, { signal })).rows()[0] ?? null,
    inScope: row => appSessionFromRow(row).ownerUserId === userId,
    paged: false,
  }));
  const source = useLiveView(feed.source, "newest", rows => sessionsNewestFirst(rows.map(appSessionFromRow).filter(row => row.id !== "")));
  return {
    source,
    sessions: source?.snapshot.rows ?? [],
    state: feed.snapshot.state,
    loading: feed.snapshot.state === "seeding",
    error: feed.snapshot.error,
    reread: feed.reseed,
  };
}

export function AppSessionsProvider({ children }: { children: ReactNode }) {
  const value = useAppSessionsFeed();
  return createElement(AppSessionsContext.Provider, { value }, children);
}

export function useAppSessions(): AppSessionsState {
  const shared = useContext(AppSessionsContext);
  const standalone = useAppSessionsFeed(shared === null);
  return shared ?? standalone;
}

export interface AppSessionDetailState {
  session: AppSessionDetailRow | null;
  loading: boolean;
  error: string;
  state: LiveState;
  reread: () => void;
}

// The list seed deliberately omits transcripts. Keep one scoped detail feed
// while a transcript is open; payload events fold in without another query.
export function useAppSessionDetail(sessionId: string): AppSessionDetailState {
  const userId = useSessionIfPresent()?.access?.userId ?? "";
  const feed = useLiveCollection<Row>(userId && sessionId ? `fleet:appSession:${userId}:${sessionId}` : null, connection => ({
    concept: APP_SESSION_CONCEPT,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.appSessionById({ sessionId }, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => id === sessionId ? (await connection.query.appSessionById({ sessionId }, { signal })).rows()[0] ?? null : null,
    inScope: row => { const session = appSessionFromRow(row); return session.id === sessionId && session.ownerUserId === userId; },
    paged: false,
  }));
  const row = feed.snapshot.rows[0];
  return { session: row ? appSessionDetailFromRow(row) : null, loading: feed.snapshot.state === "seeding",
    error: feed.snapshot.error, state: feed.snapshot.state, reread: feed.reseed };
}
