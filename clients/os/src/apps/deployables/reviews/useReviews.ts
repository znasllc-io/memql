import { useCallback, useMemo, useRef, useState } from "react";
import { isTransportInterruption, refusalSentence, renderMemQLValue, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useReading, type Reading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { describe } from "../packages/actions";
import { copyFor } from "../packages/refusals";
import {
  decisionLogFrom,
  reviewDetailFrom,
  reviewPageFrom,
  reviewSettingsFrom,
  type DecisionLogPage,
  type ReviewAct,
  type ReviewDetail,
  type ReviewFilter,
  type ReviewPage,
  type ReviewRow,
  type ReviewSettings,
  type ReviewSettingsValues,
} from "./model";

// The reads and the writes behind a store's Reviews (packs/reviewspack).
//
// NOTHING HERE IS A LIVE FEED, for the reason useAccessCodes records: the
// list, the log and the settings are builtins -- the organization gate judges
// builtins, never a query -- so each reads once, and every write reads again
// (`revision`). The section moves on what the engine answered, never on what
// it assumed a write did.

/** `builtin name(key: value, ...)`, leaving out what is not given. */
export function builtinCall(name: string, args: Record<string, unknown>): string {
  const parts = Object.entries(args)
    .filter(([, v]) => v !== undefined && v !== null && v !== "")
    .map(([k, v]) => `${k}: ${renderMemQLValue(v)}`);
  return `builtin ${name}(${parts.join(", ")})`;
}

async function answerOf(query: QueryClient, name: string, args: Record<string, unknown>, signal?: AbortSignal): Promise<Row | null> {
  try {
    const result = await query.executeNamed(name, builtinCall(name, args), signal ? { signal } : undefined);
    return result.rows()[0] ?? null;
  } catch (err: unknown) {
    if (isTransportInterruption(err)) throw new Error("The connection to the cluster dropped, so the reviews could not be read.");
    throw err;
  }
}

/**
 * The sentence a refused call gives a person. The pack's refusals are already
 * sentences; a coded one comes from the gate in front of them, in this app's
 * words for that code.
 */
export function refusalWords(err: unknown, construct: string): string {
  const { code, message } = describe(err);
  const copy = copyFor(code);
  if (copy !== null) return `${copy.title}. ${copy.next === "" ? message : copy.next}`;
  return refusalSentence(err, construct).trim().replace(/^reviews: /, "");
}

/** Where a store's reviews are read: the store, and the site the gate judges by. */
export interface ReviewsScope {
  siteId: string;
  storeId: string;
}

const PAGE = 50;

export function useReviewList(scope: ReviewsScope, filter: ReviewFilter, pages: number, revision: number): Reading<ReviewPage> {
  const connection = useOsConnection();
  const limit = Math.min(200, PAGE * Math.max(1, pages));
  const read = useMemo(() => {
    if (connection === null || scope.storeId === "") return null;
    return async (signal: AbortSignal) =>
      reviewPageFrom(await answerOf(connection.query, "reviewsList", { storeId: scope.storeId, siteId: scope.siteId, state: filter, limit }, signal));
  }, [connection, scope.storeId, scope.siteId, filter, limit]);
  return useReading(`reviews:${scope.siteId}:${scope.storeId}:${filter}:${limit}`, read, revision);
}

export function useReviewDetail(reviewId: string, revision: number): Reading<ReviewDetail> {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null || reviewId === "") return null;
    return async (signal: AbortSignal) => reviewDetailFrom(await answerOf(connection.query, "reviewsGet", { reviewId }, signal));
  }, [connection, reviewId]);
  return useReading(`review:${reviewId}`, read, revision);
}

export function useDecisionLog(scope: ReviewsScope, pages: number, revision: number): Reading<DecisionLogPage> {
  const connection = useOsConnection();
  const limit = Math.min(200, PAGE * Math.max(1, pages));
  const read = useMemo(() => {
    if (connection === null || scope.storeId === "") return null;
    return async (signal: AbortSignal) =>
      decisionLogFrom(await answerOf(connection.query, "reviewsDecisionLog", { storeId: scope.storeId, siteId: scope.siteId, limit }, signal));
  }, [connection, scope.storeId, scope.siteId, limit]);
  return useReading(`review-log:${scope.siteId}:${scope.storeId}:${limit}`, read, revision);
}

export function useReviewSettings(scope: ReviewsScope, revision: number): Reading<ReviewSettings> {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (connection === null || scope.storeId === "") return null;
    return async (signal: AbortSignal) =>
      reviewSettingsFrom(await answerOf(connection.query, "reviewsSettings", { storeId: scope.storeId, siteId: scope.siteId }, signal));
  }, [connection, scope.storeId, scope.siteId]);
  return useReading(`review-settings:${scope.siteId}:${scope.storeId}`, read, revision);
}

/**
 * How a write ended. REFUSED means the engine answered no. UNKNOWN means
 * nobody can say from here -- the reply never came -- so what the engine holds
 * is read again rather than the act tried twice.
 */
export type WriteOutcome = "done" | "refused" | "unknown";

export interface ReviewWrites {
  /** The act in flight, one at a time. */
  busy: ReviewAct | "settings" | "export" | null;
  refusal: string;
  clearRefusal: () => void;
  /** Bumped by every write, refused or not: what the engine holds is read again. */
  revision: number;
  decide: (review: ReviewRow, act: Exclude<ReviewAct, "erase">, reason: string, note: string) => Promise<WriteOutcome>;
  erase: (review: ReviewRow, note: string) => Promise<WriteOutcome>;
  saveSettings: (changes: Partial<ReviewSettingsValues>) => Promise<WriteOutcome>;
  /** The store's whole record as a file, or null when it could not be had. */
  exportStore: () => Promise<{ fileName: string; json: string } | null>;
}

export function useReviewWrites(scope: ReviewsScope): ReviewWrites {
  const connection = useOsConnection();
  const [busy, setBusy] = useState<ReviewWrites["busy"]>(null);
  const [refusal, setRefusal] = useState("");
  const [revision, setRevision] = useState(0);
  // A ref as well as `busy`: a second press before the page has drawn the
  // first finds the button still enabled, and two decisions are two rows.
  const inFlight = useRef(false);

  const run = useCallback(
    async (act: NonNullable<ReviewWrites["busy"]>, construct: string, work: (query: QueryClient) => Promise<void>): Promise<WriteOutcome> => {
      if (inFlight.current) return "refused";
      setRefusal("");
      if (connection === null) {
        setRefusal("Not connected to the cluster, so nothing was written.");
        return "refused";
      }
      inFlight.current = true;
      setBusy(act);
      try {
        await work(connection.query);
        return "done";
      } catch (err: unknown) {
        if (isTransportInterruption(err)) {
          setRefusal("The connection dropped before the cluster answered, so this may have been saved. What the cluster holds is shown again.");
          return "unknown";
        }
        setRefusal(refusalWords(err, construct) || "That did not complete. Try again.");
        return "refused";
      } finally {
        inFlight.current = false;
        setBusy(null);
        setRevision((n) => n + 1);
      }
    },
    [connection],
  );

  const decide = useCallback(
    (review: ReviewRow, act: Exclude<ReviewAct, "erase">, reason: string, note: string) =>
      run(act, "reviewsDecide", async (query) => {
        // THE WORDS READ ARE NAMED BACK: the revision and the fingerprint of
        // what was on screen, so a decision never lands on words nobody read.
        await query.executeNamed("reviewsDecide", builtinCall("reviewsDecide", {
          reviewId: review.id, action: act, reason, note: note.trim(), revision: review.revision, contentHash: review.contentHash,
        }));
      }),
    [run],
  );

  const erase = useCallback(
    (review: ReviewRow, note: string) =>
      run("erase", "reviewsEraseReviewer", async (query) => {
        await query.executeNamed("reviewsEraseReviewer", builtinCall("reviewsEraseReviewer", { reviewId: review.id, note: note.trim() }));
      }),
    [run],
  );

  const saveSettings = useCallback(
    (changes: Partial<ReviewSettingsValues>) =>
      run("settings", "reviewsUpdateSettings", async (query) => {
        await query.executeNamed("reviewsUpdateSettings", builtinCall("reviewsUpdateSettings", { storeId: scope.storeId, siteId: scope.siteId, ...changes }));
      }),
    [run, scope.storeId, scope.siteId],
  );

  const exportStore = useCallback(async () => {
    let file: { fileName: string; json: string } | null = null;
    await run("export", "reviewsExportStore", async (query) => {
      const row = await answerOf(query, "reviewsExportStore", { storeId: scope.storeId, siteId: scope.siteId });
      const json = typeof row?.["json"] === "string" ? row["json"] : "";
      if (json === "") throw new Error("The cluster answered without the file. Try again.");
      file = { fileName: typeof row?.["fileName"] === "string" && row["fileName"] !== "" ? row["fileName"] : "reviews.json", json };
    });
    return file;
  }, [run, scope.storeId, scope.siteId]);

  return { busy, refusal, clearRefusal: useCallback(() => setRefusal(""), []), revision, decide, erase, saveSettings, exportStore };
}
