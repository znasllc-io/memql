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
  wholesaleBuyersFrom,
  wholesaleDocumentsFrom,
  wholesaleDocumentVersionFrom,
  wholesalePageFrom,
  wholesaleSettingsFrom,
  type DocumentKind,
  type DocumentKindView,
  type EmailKind,
  type DocumentVersion,
  type WholesaleApplication,
  type WholesaleBuyer,
  type WholesaleBuyersPage,
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
        rows: [{ id: METADATA, metadata: { counts: page.counts, truncated: page.truncated, operator: page.operator }, revision: readRevision }, ...page.rows.map(item => ({ id: item.id, item }))],
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

type BuyerEntry = { id: string; item?: WholesaleBuyer; metadata?: { truncated: boolean; operator: boolean }; revision?: number };

/**
 * The store's buyers -- every approved application -- newest first, ten at a
 * time, paged through the kit's `LoadMore` as the applications are.
 */
export function useWholesaleBuyers(scope: WholesaleScope, search: string, revision: number) {
  const visible = useContext(LiveReadVisibility);
  const query = useDebouncedValue(search.trim(), 250);
  const current = useRef(revision);
  current.current = revision;
  const key = JSON.stringify([scope.siteId, scope.storeId, query]);
  const feed = usePagedCollection<BuyerEntry>(scope.storeId ? `wholesale:buyers:${key}` : null, connection => ({
    concept: "v1:wholesale:application", actions: [],
    seed: async (cursor, signal) => {
      const readRevision = current.current;
      const page: WholesaleBuyersPage = wholesaleBuyersFrom(await answerOf(connection.query, "wholesaleBuyers", "the buyers", { ...scope, search: query || undefined, limit: 10, after: cursor || undefined }, signal));
      return {
        rows: [{ id: METADATA, metadata: { truncated: page.truncated, operator: page.operator }, revision: readRevision }, ...page.rows.map(item => ({ id: item.id, item }))],
        nextCursor: page.next,
      };
    },
  }));
  const metadata = feed.snapshot.rows.find(row => row.id === METADATA);
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

/** The store's three documents, each with its current version and its history. */
export function useWholesaleDocuments(scope: WholesaleScope, revision: number): Reading<DocumentKindView[]> {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null || scope.storeId === "") return null;
    return async (signal: AbortSignal) =>
      wholesaleDocumentsFrom(await answerOf(connection.query, "wholesaleDocuments", "the documents", { siteId: scope.siteId, storeId: scope.storeId }, signal));
  }, [connection, scope.storeId, scope.siteId]);
  return useReading(`wholesale-documents:${scope.siteId}:${scope.storeId}`, read, revision);
}

/** One version of one of the store's documents, its text included. */
export function useWholesaleDocumentVersion(scope: WholesaleScope, versionId: string): Reading<DocumentVersion> {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null || scope.storeId === "" || versionId === "") return null;
    return async (signal: AbortSignal) =>
      wholesaleDocumentVersionFrom(await answerOf(connection.query, "wholesaleDocuments", "this version", { siteId: scope.siteId, storeId: scope.storeId, versionId }, signal));
  }, [connection, scope.storeId, scope.siteId, versionId]);
  return useReading(`wholesale-document:${scope.siteId}:${scope.storeId}:${versionId}`, read, 0);
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

/** What a read found after a write whose answer was lost: there, not there, or it could not be read. */
export type Found = "there" | "absent" | "unread";

/**
 * THE READS THAT SETTLE A LOST ANSWER. A dropped connection after Add buyer or
 * Publish leaves the write's outcome unknown, and pressing again blind could
 * write it twice (versions are append-only). So the page asks the store what
 * it holds, with the same reads every tab makes, and says which it found.
 */
export function useWholesaleChecks(scope: WholesaleScope) {
  const connection = useOsConnection();
  /** Whether the store holds an open application for this address (the engine's one-per-address rule). */
  const findBuyer = useCallback(async (applicantEmail: string): Promise<{ found: Found; applicationId?: string }> => {
    if (connection === null) return { found: "unread" };
    const wanted = applicantEmail.trim().toLowerCase();
    try {
      const page = wholesalePageFrom(await answerOf(connection.query, "wholesaleList", "the applications", { ...scope, stage: "all", search: applicantEmail.trim(), limit: 50 }));
      const open = page.rows.find((r) => r.applicantEmail.trim().toLowerCase() === wanted && (r.stage === "waitingForTerms" || r.stage === "readyToReview" || r.stage === "approved"));
      return open ? { found: "there", applicationId: open.id } : { found: "absent" };
    } catch {
      return { found: "unread" };
    }
  }, [connection, scope.siteId, scope.storeId]);
  /** Whether the kind's current version is this label and this text. */
  const findVersion = useCallback(async (kind: DocumentKind, label: string, body: string): Promise<Found> => {
    if (connection === null) return "unread";
    try {
      const kinds = wholesaleDocumentsFrom(await answerOf(connection.query, "wholesaleDocuments", "the documents", { siteId: scope.siteId, storeId: scope.storeId }));
      const current = kinds.find((k) => k.kind === kind)?.current ?? null;
      return current !== null && current.label === label.trim() && current.body === body ? "there" : "absent";
    } catch {
      return "unread";
    }
  }, [connection, scope.siteId, scope.storeId]);
  return { findBuyer, findVersion };
}

/** A write's outcome, and the sentence of its refusal for a page that draws it in place. */
export interface WriteResult {
  outcome: WriteOutcome;
  /** The engine's sentence, or the page's own for no connection; "" when it went. */
  sentence: string;
}

export interface WholesaleWrites {
  /** The write in flight, one at a time. */
  busy: "decide" | "retry" | "settings" | "setUp" | "add" | "remove" | "resend" | "publish" | "export" | null;
  refusal: string;
  clearRefusal: () => void;
  /** Bumped by every write, refused or not: what the engine holds is read again. */
  revision: number;
  decide(applicationId: string, transition: "approve" | "reject", note: string): Promise<WriteOutcome>;
  retryAccess(applicationId: string): Promise<WriteOutcome>;
  updateSettings(changes: Partial<WholesaleSettingsChanges>): Promise<WriteOutcome>;
  setUpStore(): Promise<WriteOutcome>;
  /** Its refusal is the wizard's to draw, on the step it answers: `refusal` is left alone. */
  addBuyer(input: { companyName: string; applicantName: string; applicantEmail: string }): Promise<WriteResult & { applicationId?: string }>;
  removeBuyer(applicationId: string, note: string): Promise<WriteOutcome>;
  resendEmail(applicationId: string, kind: EmailKind): Promise<WriteOutcome>;
  /** Its refusal is the wizard's to draw, on the step it answers: `refusal` is left alone. */
  publishDocument(kind: DocumentKind, label: string, body: string): Promise<WriteResult>;
  /**
   * The store's every record as one file, or the sentence why not -- drawn by
   * Settings beside Download every record, where the act was taken: `refusal`
   * is left alone.
   */
  exportStore(): Promise<{ fileName: string; json: string } | { refusal: string }>;
}

export function useWholesaleWrites(scope: WholesaleScope): WholesaleWrites {
  const connection = useOsConnection();
  const [busy, setBusy] = useState<WholesaleWrites["busy"]>(null);
  const [refusal, setRefusal] = useState("");
  const [revision, setRevision] = useState(0);
  // A ref as well as `busy`: a second press before the page has drawn the
  // first finds the button still enabled, and two decisions are two rows.
  const inFlight = useRef(false);

  // Every write, and the reply's one row for the writes that answer
  // something the page goes on with (the new application's id, the file).
  //
  // IN PLACE (`inPlace`): the page that took the act draws its sentence where
  // it was taken, and the shared `refusal` -- drawn above a bar -- is left
  // alone. A lost answer is then the page's to settle too: it says nothing
  // here of what is "shown again", because a wizard re-reads nothing until it
  // asks.
  const call = useCallback(
    async (kind: NonNullable<WholesaleWrites["busy"]>, name: string, args: Record<string, unknown>, given: readonly string[] = [], inPlace = false): Promise<{ outcome: WriteOutcome; row: Row | null; sentence: string }> => {
      if (inFlight.current) return { outcome: "refused", row: null, sentence: "" };
      const say = (sentence: string) => { if (!inPlace) setRefusal(sentence); };
      say("");
      if (connection === null) {
        const sentence = "Not connected to the cluster, so nothing was written.";
        say(sentence);
        return { outcome: "refused", row: null, sentence };
      }
      inFlight.current = true;
      setBusy(kind);
      try {
        const result = await connection.query.executeNamed(name, builtinCall(name, args, given));
        return { outcome: "done", row: result.rows()[0] ?? null, sentence: "" };
      } catch (err: unknown) {
        if (isTransportInterruption(err)) {
          say("The connection dropped before the cluster answered, so this may have been saved. What the cluster holds is shown again.");
          return { outcome: "unknown", row: null, sentence: "" };
        }
        // THE ENGINE'S SENTENCE, as it arrived.
        const sentence = refusalWords(err, name) || "That did not complete. Try again.";
        say(sentence);
        return { outcome: "refused", row: null, sentence };
      } finally {
        inFlight.current = false;
        setBusy(null);
        setRevision((n) => n + 1);
      }
    },
    [connection],
  );
  const run = useCallback(
    async (kind: NonNullable<WholesaleWrites["busy"]>, name: string, args: Record<string, unknown>, given: readonly string[] = []): Promise<WriteOutcome> =>
      (await call(kind, name, args, given)).outcome,
    [call],
  );

  // THE DECIDER IS NEVER SENT: the engine records the caller, and ignores
  // principalKind and decidedBy for the callers that still send them.
  const decide = useCallback(
    (applicationId: string, transition: "approve" | "reject", note: string) =>
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

  // A BUYER ADDED BY HAND is an application the engine writes and names: the
  // page opens it by the id the reply gives, whole.
  const addBuyer = useCallback(
    async (input: { companyName: string; applicantName: string; applicantEmail: string }) => {
      const { outcome, row, sentence } = await call("add", "wholesaleAddBuyer", {
        siteId: scope.siteId, storeId: scope.storeId,
        companyName: input.companyName.trim(), applicantName: input.applicantName.trim(), applicantEmail: input.applicantEmail.trim(),
      }, [], true);
      const applicationId = typeof row?.["applicationId"] === "string" && row["applicationId"] !== "" ? row["applicationId"] : undefined;
      return applicationId === undefined ? { outcome, sentence } : { outcome, sentence, applicationId };
    },
    [call, scope.siteId, scope.storeId],
  );

  // A REMOVAL IS ITS OWN ACT (wholesaleRemoveBuyer), a revoke on an approved
  // application recorded by the caller with the note.
  const removeBuyer = useCallback(
    (applicationId: string, note: string) => run("remove", "wholesaleRemoveBuyer", { applicationId, note: note.trim() }, ["note"]),
    [run],
  );

  const resendEmail = useCallback(
    (applicationId: string, kind: EmailKind) => run("resend", "wholesaleResendEmail", { applicationId, kind }),
    [run],
  );

  // THE TEXT AS GIVEN: the engine stores it exactly and fingerprints its bytes.
  const publishDocument = useCallback(
    async (kind: DocumentKind, label: string, body: string): Promise<WriteResult> => {
      const { outcome, sentence } = await call("publish", "wholesalePublishDocument", { siteId: scope.siteId, storeId: scope.storeId, kind, label: label.trim(), body }, [], true);
      return { outcome, sentence };
    },
    [call, scope.siteId, scope.storeId],
  );

  // THE FILE IS MADE IN THE BROWSER from the reply, and kept nowhere else.
  const exportStore = useCallback(async () => {
    const { outcome, row, sentence } = await call("export", "wholesaleExportStore", { siteId: scope.siteId, storeId: scope.storeId }, [], true);
    // A read writes nothing, so a lost answer is simply not the file.
    if (outcome === "unknown") return { refusal: "The connection dropped before the cluster answered, so the file did not arrive. Try again." };
    if (outcome !== "done") return { refusal: sentence };
    const json = typeof row?.["json"] === "string" ? row["json"] : "";
    if (json === "") return { refusal: "The cluster answered without the file. Try again." };
    return { fileName: typeof row?.["fileName"] === "string" && row["fileName"] !== "" ? row["fileName"] : "wholesale.json", json };
  }, [call, scope.siteId, scope.storeId]);

  return { busy, refusal, clearRefusal: useCallback(() => setRefusal(""), []), revision, decide, retryAccess, updateSettings, setUpStore, addBuyer, removeBuyer, resendEmail, publishDocument, exportStore };
}
