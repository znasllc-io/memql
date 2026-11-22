import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { isTransportInterruption, refusalSentence, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import type { ShareChoice } from "../machines/useMachineWrites";
import { activityFrom, keyCardFrom, keyListsFrom, refusalWords, UNANSWERED, type KeyActivity, type KeyCard, type KeyLists } from "./words";

// The reads and the writes behind Fleet > API keys (design record
// 2026-11-21-vendor-accounts-and-api-keys, U1 to U3).
//
// NOTHING HERE IS A LIVE FEED. A key's two rows publish no graph events: they
// hold a sealed credential and what the vendor said about it, and a client is
// not handed either. So the lists are READ, read again after every write this
// page makes, when the section is shown again, and on the shell's recovery
// cadence while it is open (`AutoRefresh`, beside the list), because a key can
// change with no act here: a call it paid for was refused, or the daily check
// found it revoked.
//
// A FAILED READ IS NOT AN EMPTY LIST, and a later one does not take the keys
// off the screen. The lists last read stay, and `behind` says they are not
// current.
//
// THE KEY A PERSON TYPES IS AN ARGUMENT OF `add` AND NOTHING ELSE. It is not
// state on this hook, it is in no reply, and a refusal that somehow repeated
// it would be cut before it was shown (`refusalWords`).

export type KeysState = "loading" | "failed" | "ready";

/** How a write ended. `unknown` is a reply that never came: the write may have landed. */
export type WriteOutcome =
  | { outcome: "done"; card: KeyCard }
  | { outcome: "refused"; sentence: string }
  | { outcome: "unknown"; sentence: string };

export interface KeyPatch {
  label?: string;
  isDefault?: boolean;
  autoUse?: boolean;
}

export interface VendorKeys {
  state: KeysState;
  mine: KeyCard[];
  lent: KeyCard[];
  /** Every active key on the cluster. Empty unless a cluster owner asked. */
  all: KeyCard[];
  /** Why the first read failed, in the cluster's words. */
  error: string;
  /** A later read failed: the lists are the last ones read. The reason, for a hover. */
  behind: string;
  reload: () => void;
  /** Whether every key on the cluster is being asked for. */
  wantAll: boolean;
  setWantAll: (all: boolean) => void;
  /** The write in flight, "" when none: "add", "update:<id>", "check:<id>", "remove:<id>", "sharing:<id>". */
  busy: string;
  add: (vendor: string, label: string, key: string) => Promise<WriteOutcome>;
  update: (keyId: string, patch: KeyPatch) => Promise<WriteOutcome>;
  check: (keyId: string) => Promise<WriteOutcome>;
  remove: (keyId: string) => Promise<WriteOutcome>;
  setSharing: (keyId: string, choice: ShareChoice) => Promise<WriteOutcome>;
}

function readFailure(err: unknown): string {
  if (isTransportInterruption(err)) return "The connection to the cluster dropped, so your API keys could not be read.";
  return refusalSentence(err, "vendorKeysForActor").trim() || "The cluster refused and said nothing about why.";
}

export function useVendorKeys(): VendorKeys {
  const connection = useOsConnection();
  const { access } = useSession();
  const [wantAll, setWantAll] = useState(false);
  // A REVISION, NOT `reread()`: `reread` is ignored while a read is out, and a
  // read that started before a write landed answers with the world before it.
  // A revision remembers one follow-up read.
  const [revision, setRevision] = useState(0);
  const [busy, setBusy] = useState("");
  // A ref as well as `busy`: a second press before the page has drawn the
  // first finds the control still there, and two adds are two keys.
  const inFlight = useRef(false);

  const read = useMemo(() => {
    if (connection === null) return null;
    return async (signal: AbortSignal): Promise<KeyLists> => {
      let result: Result;
      try {
        result = await connection.query.vendorKeysForActor(wantAll ? { all: true } : {}, { signal, priority: "background" });
      } catch (err: unknown) {
        throw new Error(readFailure(err));
      }
      const lists = keyListsFrom(result.rows());
      if (lists === null) throw new Error("The cluster answered without a list of keys.");
      return lists;
    };
  }, [connection, wantAll]);
  const reading = useReading<KeyLists>(`fleet:keys:${access?.userId ?? ""}:${wantAll ? "all" : "mine"}`, read, revision);

  // The lists last read, kept through a failed read and through a change of
  // question (a cluster owner asking for every key, or going back).
  const [held, setHeld] = useState<KeyLists | null>(null);
  useEffect(() => {
    if (reading.state === "read" && reading.value !== null) setHeld(reading.value);
  }, [reading.state, reading.value]);

  const reload = useCallback(() => setRevision((n) => n + 1), []);

  const run = useCallback(
    async (act: keyof typeof UNANSWERED, tag: string, construct: string, typed: string, work: (query: QueryClient) => Promise<Result>): Promise<WriteOutcome> => {
      // One is out already: this press did nothing.
      if (inFlight.current) return { outcome: "refused", sentence: "Another change is still being saved. Try again in a moment." };
      if (connection === null) return { outcome: "refused", sentence: "Not connected to the cluster, so nothing was written." };
      inFlight.current = true;
      setBusy(tag);
      try {
        const result = await work(connection.query);
        const card = keyCardFrom(result.rows()[0] ?? null);
        // Accepted, with no card to show: it landed, and the list will say how.
        if (card === null) return { outcome: "unknown", sentence: "The cluster accepted that but did not say what it saved. Check the list." };
        return { outcome: "done", card };
      } catch (err: unknown) {
        // A LOST REPLY IS NOT A REFUSAL: a write's outcome is reconciled before
        // any retry, and the socket's own words mean nothing to a person.
        if (isTransportInterruption(err)) return { outcome: "unknown", sentence: UNANSWERED[act] ?? UNANSWERED["update"]! };
        return { outcome: "refused", sentence: refusalWords(err, construct, typed) };
      } finally {
        inFlight.current = false;
        setBusy("");
        setRevision((n) => n + 1);
      }
    },
    [connection],
  );

  const add = useCallback(
    (vendor: string, label: string, key: string) => run("add", "add", "vendorKeyAdd", key, (query) => query.vendorKeyAdd({ vendor, label, key })),
    [run],
  );
  const update = useCallback(
    (keyId: string, patch: KeyPatch) => run("update", `update:${keyId}`, "vendorKeyUpdate", "", (query) => query.vendorKeyUpdate({ keyId, ...patch })),
    [run],
  );
  const check = useCallback((keyId: string) => run("check", `check:${keyId}`, "vendorKeyCheck", "", (query) => query.vendorKeyCheck({ keyId })), [run]);
  const remove = useCallback((keyId: string) => run("remove", `remove:${keyId}`, "vendorKeyRemove", "", (query) => query.vendorKeyRemove({ keyId })), [run]);
  const setSharing = useCallback(
    (keyId: string, choice: ShareChoice) =>
      run("sharing", `sharing:${keyId}`, "vendorKeySetSharing", "", (query) =>
        query.vendorKeySetSharing({
          keyId,
          // A key is its owner's alone or lent to named people. It is never lent to everyone.
          mode: choice.mode === "people" ? "people" : "owner",
          paused: choice.paused === true,
          userIds: choice.userIds,
          groupIds: choice.groupIds,
          pausedUserIds: choice.pausedUserIds ?? [],
          pausedGroupIds: choice.pausedGroupIds ?? [],
        }),
      ),
    [run],
  );

  const lists = held ?? { mine: [], lent: [], all: [] };
  return {
    state: held !== null ? "ready" : reading.state === "failed" ? "failed" : "loading",
    mine: lists.mine,
    lent: lists.lent,
    all: wantAll ? lists.all : [],
    error: held === null && reading.state === "failed" ? reading.error : "",
    behind: held !== null && reading.state === "failed" ? reading.error : "",
    reload,
    wantAll,
    setWantAll,
    busy,
    add,
    update,
    check,
    remove,
    setSharing,
  };
}

export interface ActivityReading {
  state: "loading" | "failed" | "ready";
  activity: KeyActivity | null;
  error: string;
  reread: () => void;
}

/**
 * What one of the reader's own keys paid for this month.
 *
 * Read once when asked for and again when `revision` moves, never on a timer:
 * it adds up every call the key paid for in the month, and a list that asked
 * for it every few seconds would be reading the month over and over.
 */
export function useKeyActivity(keyId: string, enabled: boolean, revision: string | number = 0): ActivityReading {
  const connection = useOsConnection();
  const read = useMemo(() => {
    if (!enabled || connection === null || keyId === "") return null;
    return async (signal: AbortSignal): Promise<KeyActivity> => {
      let result: Result;
      try {
        result = await connection.query.vendorKeyActivity({ keyId }, { signal, priority: "background" });
      } catch (err: unknown) {
        if (isTransportInterruption(err)) throw new Error("The connection to the cluster dropped.");
        throw new Error(refusalSentence(err, "vendorKeyActivity").trim() || "The cluster refused and said nothing about why.");
      }
      const activity = activityFrom(result.rows());
      if (activity === null) throw new Error("The cluster answered without the key's activity.");
      return activity;
    };
  }, [connection, keyId, enabled]);
  const reading = useReading<KeyActivity>(`fleet:keys:activity:${keyId}`, read, revision);
  return {
    state: reading.value !== null ? "ready" : reading.state === "failed" ? "failed" : "loading",
    activity: reading.value,
    error: reading.state === "failed" ? reading.error : "",
    reread: reading.reread,
  };
}
