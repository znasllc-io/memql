import { useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import { isTransportInterruption, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

import { usePagedCollection } from "../../../live/useLiveCollection";
import { LiveReadVisibility } from "../../../live/useGraphRevision";
import { useDebouncedValue } from "../../../logs/useDebouncedValue";
import { useReading, type Reading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { builtinCall, refusalWords, type WriteOutcome } from "../reviews/useReviews";
import {
  wholesaleApplicationFrom,
  wholesalePageFrom,
  wholesaleSettingsFrom,
  type WholesaleApplication,
  type WholesaleFilter,
  type WholesalePage,
  type WholesaleRow,
  type WholesaleSettings,
  type WholesaleSettingsChanges,
} from "./model";

// The reads and the writes behind a store's Wholesale section
// (packs/wholesalepack: reads.go, settings.go, application.go, entitlement.go).
//
// NOTHING HERE IS A LIVE FEED, for the reason the Reviews hooks record: these
// are builtins, which the organization gate judges, so each reads once and
// every write reads again (`revision`). The section moves on what the engine
// answered, never on what it assumed a write did.
//
// THE CALL WORDS ARE THE REVIEWS HOOKS' OWN (`builtinCall`, `refusalWords`,
// `WriteOutcome`), imported rather than copied: one spelling of a builtin call
// and of a refusal for the whole app.

/** Where a store's applications are read: the store, and the site the gate judges by. */
export interface WholesaleScope {
  siteId: string;
  storeId: string;
}

/**
 * One read's answer. A read that fails says so in the engine's words, or, with
 * none, names what could not be read (`what`: "the applications", "this
 * application", "the settings").
 */
async function answerOf(query: QueryClient, name: string, what: string, args: Record<string, unknown>, signal?: AbortSignal): Promise<Row | null> {
  try {
    const result = await query.executeNamed(name, builtinCall(name, args), signal ? { signal } : undefined);
    return result.rows()[0] ?? null;
  } catch (err: unknown) {
    if (isTransportInterruption(err)) throw new Error(`The connection to the cluster dropped, so ${what} could not be read.`);
    const said = refusalWords(err, name);
    throw new Error(said || `${what.charAt(0).toUpperCase()}${what.slice(1)} could not be read.`);
  }
}

type Entry = { id: string; item?: WholesaleRow; metadata?: Omit<WholesalePage, "rows" | "next">; revision?: number };
const METADATA = "wholesale-list-metadata";

/**
 * One stage of the store's applications, ten at a time, the engine's order:
 * Ready to review oldest first, every other stage newest first. `next` pages
 * it through the kit's `LoadMore`, as the Reviews lists page.
 */
export function useWholesaleList(scope: WholesaleScope, stage: WholesaleFilter, search: string, revision: number) {
  const visible = useContext(LiveReadVisibility);
  const query = useDebouncedValue(search.trim(), 250);
  // THE WRITES' REVISION IS THE ONE INVALIDATION: each page remembers the
  // revision it was read at, and a list read before the latest write -- one a
  // tab released and kept, say -- is read again when it is next shown.
  const current = useRef(revision);
  current.current = revision;
  const key = JSON.stringify([scope.siteId, scope.storeId, stage, query]);
  const feed = usePagedCollection<Entry>(scope.storeId ? `wholesale:list:${key}` : null, connection => ({
    concept: "v1:wholesale:application", actions: [],
    seed: async (cursor, signal) => {
      const readRevision = current.current;
      const page = wholesalePageFrom(await answerOf(connection.query, "wholesaleList", "the applications", { ...scope, stage, search: query || undefined, limit: 10, after: cursor || undefined }, signal));
      return {
        rows: [{ id: METADATA, metadata: { counts: page.counts, truncated: page.truncated }, revision: readRevision }, ...page.rows.map(item => ({ id: item.id, item }))],
        nextCursor: page.next,
      };
    },
  }));
  const metadata = feed.snapshot.rows.find(row => row.id === METADATA);
  // An inactive window does no network work.
  useEffect(() => { if (visible && metadata && metadata.revision !== revision) feed.reseed(); }, [visible, metadata?.revision, revision, feed.reseed]);
  const rows = feed.snapshot.rows.flatMap(row => row.item ? [row.item] : []);
  return {
    ...feed,
    value: metadata?.metadata ? { ...metadata.metadata, rows } : null,
    state: feed.snapshot.error ? "failed" : feed.snapshot.state === "live" ? "read" : "reading",
    error: feed.snapshot.error,
    reread: feed.reseed,
  };
}

export function useWholesaleApplication(applicationId: string, revision: number): Reading<WholesaleApplication> {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null || applicationId === "") return null;
    // THE ID AS THE LIST GAVE IT, whole.
    return async (signal: AbortSignal) => wholesaleApplicationFrom(await answerOf(connection.query, "wholesaleGet", "this application", { applicationId }, signal));
  }, [connection, applicationId]);
  return useReading(`wholesale-application:${applicationId}`, read, revision);
}

export function useWholesaleSettings(scope: WholesaleScope, revision: number): Reading<WholesaleSettings> {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null || scope.storeId === "") return null;
    return async (signal: AbortSignal) =>
      wholesaleSettingsFrom(await answerOf(connection.query, "wholesaleStoreSettings", "the settings", { siteId: scope.siteId, storeId: scope.storeId }, signal));
  }, [connection, scope.storeId, scope.siteId]);
  return useReading(`wholesale-settings:${scope.siteId}:${scope.storeId}`, read, revision);
}

export interface WholesaleWrites {
  /** The write in flight, one at a time. */
  busy: "decide" | "retry" | "settings" | "setUp" | null;
  refusal: string;
  clearRefusal: () => void;
  /** Bumped by every write, refused or not: what the engine holds is read again. */
  revision: number;
  decide(applicationId: string, transition: "approve" | "reject" | "revoke", note: string): Promise<WriteOutcome>;
  retryAccess(applicationId: string): Promise<WriteOutcome>;
  updateSettings(changes: Partial<WholesaleSettingsChanges>): Promise<WriteOutcome>;
  setUpStore(): Promise<WriteOutcome>;
}

export function useWholesaleWrites(scope: WholesaleScope): WholesaleWrites {
  const connection = useOsConnection();
  const [busy, setBusy] = useState<WholesaleWrites["busy"]>(null);
  const [refusal, setRefusal] = useState("");
  const [revision, setRevision] = useState(0);
  // A ref as well as `busy`: a second press before the page has drawn the
  // first finds the button still enabled, and two decisions are two rows.
  const inFlight = useRef(false);

  const run = useCallback(
    async (kind: NonNullable<WholesaleWrites["busy"]>, name: string, args: Record<string, unknown>, given: readonly string[] = []): Promise<WriteOutcome> => {
      if (inFlight.current) return "refused";
      setRefusal("");
      if (connection === null) {
        setRefusal("Not connected to the cluster, so nothing was written.");
        return "refused";
      }
      inFlight.current = true;
      setBusy(kind);
      try {
        await connection.query.executeNamed(name, builtinCall(name, args, given));
        return "done";
      } catch (err: unknown) {
        if (isTransportInterruption(err)) {
          setRefusal("The connection dropped before the cluster answered, so this may have been saved. What the cluster holds is shown again.");
          return "unknown";
        }
        // THE ENGINE'S SENTENCE, as it arrived.
        setRefusal(refusalWords(err, name) || "That did not complete. Try again.");
        return "refused";
      } finally {
        inFlight.current = false;
        setBusy(null);
        setRevision((n) => n + 1);
      }
    },
    [connection],
  );

  // THE DECIDER IS NEVER SENT: the engine records the caller, and ignores
  // principalKind and decidedBy for the callers that still send them.
  const decide = useCallback(
    (applicationId: string, transition: "approve" | "reject" | "revoke", note: string) =>
      run("decide", "wholesaleRecordDecision", { applicationId, transition, note: note.trim() }, ["note"]),
    [run],
  );

  const retryAccess = useCallback((applicationId: string) => run("retry", "wholesaleRetryAccess", { applicationId }), [run]);

  // ONLY WHAT CHANGED, an emptied route as "": the engine merges a change and
  // keeps every field a call does not name.
  const updateSettings = useCallback(
    (changes: Partial<WholesaleSettingsChanges>) =>
      run("settings", "wholesaleUpdateSettings", { storeId: scope.storeId, siteId: scope.siteId, ...changes }, Object.keys(changes)),
    [run, scope.storeId, scope.siteId],
  );

  // SETTING A STORE UP is the cluster operators' alone: the row is made closed,
  // with no route, for this site and this store.
  const setUpStore = useCallback(
    () => run("setUp", "wholesaleSetSettings", { storeId: scope.storeId, siteId: scope.siteId, applicationsOpen: false, entitlementAdapter: "" }, ["entitlementAdapter"]),
    [run, scope.storeId, scope.siteId],
  );

  return { busy, refusal, clearRefusal: useCallback(() => setRefusal(""), []), revision, decide, retryAccess, updateSettings, setUpStore };
}
