import { useCallback, useEffect, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../kit/rows";
import { useOsConnection } from "../live/connection";
import { onAppResume } from "../live/resume";
import { useReadEffect } from "../live/useReadEffect";
import { useSession } from "./access";
import { useConnectionRecovery, useConnectionStatus } from "./connection";

// WHETHER THIS PERSON MAY ENTER MEMQL OS (design record
// 2026-11-24-desktop-access, sections 3 and 4).
//
// The `desktopAccess` builtin answers for the CALLER only: whether they are a
// cluster owner or developer (exempt), whether they have model access set up
// (machines, apps, keys, the cluster's account, asleep or not), and, when they
// have none, a reference whose log line says what is missing. CoreGate draws
// the held page from a held answer.
//
// ===========================================================================
// WHEN IT ASKS AGAIN
// ===========================================================================
// Nothing that fixes a held person reaches their stream: key rows publish no
// graph event, a machine registration is visible only to its owner and the
// cluster's owners, grants are readable from admin rank. So the page asks:
//   - on window focus, tab return and network return (`onAppResume`);
//   - when the connection returns (`managed` stream back to "connected");
//   - every 30 seconds while held and the document is visible;
//   - on `recheck()`, which the page calls after a key is added on it.
// A check asked for while one is out runs once more after it, never beside it
// (`useReadEffect`): two checks in flight for somebody newly held would mint
// two references, and the page would show the one Logs found second.
//
// ===========================================================================
// THE REFERENCE TRAVELS WITH THE PAGE
// ===========================================================================
// A check passes back the reference and digest of the last held answer, so a
// check that still holds the person keeps their reference and writes nothing
// new (the engine writes a new line under the same reference only when the
// reasons changed). Nothing lives on a replica, so any replica can answer. An
// admitted answer drops them: the next hold is a new showing with a new line.
// They belong to the signed-in person, and are dropped when the session names
// somebody else (the engine's digest binds the person as well, so a passed
// reference never answers for a second person either way).
//
// ===========================================================================
// WHAT A CHECK THAT FAILS MEANS
// ===========================================================================
// The engine answers a read it could not make with an ERROR, never a row. The
// first check failing is "unknown", which lets the person in: an outage is not
// evidence that a person lacks access, and the model calls are still refused
// on the server. A later check that fails keeps the answer the page already
// has, so a held person is not let in by a dropped call and an admitted person
// is not thrown out by one. A reply with no row is no answer, and reads the
// same as a failure.
//
// `settled` says whether a check has come back at all, with an answer or a
// failure. The gate's first entry waits on it for up to three seconds from the
// gate's mount, the bound the readiness feed has. A held person whose answer
// lands within it sees the page and not the desktop; one whose answer comes
// later sees the desktop replaced by the page. A check that fails lets the
// person in at once rather than after the bound.
//
// Regression tests: test/chrome/accessHeld.test.tsx ("waits at the door for the
// first check" pins the wait).

/** One model source the person has set up, as the engine lists it. */
export type AccessSourceRow = {
  kind: string;
  state: string;
  own: boolean;
  lenderUserId?: string;
  label?: string;
  /** The app id (claude-code, codex) of an app source. */
  app?: string;
};

export type DesktopAccessAnswer =
  | { state: "unknown" }
  | { state: "admitted"; exempt: boolean; sources: AccessSourceRow[] }
  | { state: "held"; reference: string; reasonsDigest: string };

/** How often a held page asks again while it is visible. */
export const ACCESS_RECHECK_MS = 30_000;

const UNKNOWN: DesktopAccessAnswer = { state: "unknown" };

interface Reading {
  /** The signed-in person the answer is for; "" when it was read before the session named them. */
  person: string;
  answer: DesktopAccessAnswer;
  settled: boolean;
}

export function useDesktopAccess(enabled: boolean): DesktopAccessAnswer & { recheck(): void; settled: boolean } {
  const connection = useOsConnection();
  const status = useConnectionStatus();
  const { managed } = useConnectionRecovery();
  const person = useSession().access?.userId ?? "";
  const [reading, setReading] = useState<Reading>({ person: "", answer: UNKNOWN, settled: false });
  const [asks, setAsks] = useState(0);
  const recheck = useCallback(() => setAsks((n) => n + 1), []);
  // What the last held answer carried, handed back by the next check. A ref:
  // it is an argument of the next read, not something to draw.
  const held = useRef<{ reference: string; reasonsDigest: string } | null>(null);

  // THE SESSION NAMING SOMEBODY ELSE. The first person the session names is
  // whoever an answer read before that was for (the call is the bearer's);
  // after that, a different person drops what was held and asks for them.
  // Declared before the read, so a check it asks for starts with nothing held.
  const known = useRef(person);
  useEffect(() => {
    if (person === "" || person === known.current) return;
    const before = known.current;
    known.current = person;
    if (before === "") return;
    held.current = null;
    setReading({ person, answer: UNKNOWN, settled: false });
    recheck();
  }, [person, recheck]);

  const reachable = enabled && connection !== null && (!managed || status === "connected");
  useReadEffect(
    async (signal) => {
      if (!reachable || connection === null) return;
      const askedFor = known.current;
      const last = held.current;
      let next: DesktopAccessAnswer | null = null;
      try {
        const result = await connection.query.desktopAccess(
          { reference: last?.reference ?? "", reasonsDigest: last?.reasonsDigest ?? "" },
          // The first check is on the way in; later ones are refreshes.
          { signal, ...(reading.settled ? { priority: "background" as const } : {}) },
        );
        next = answerFrom(result.rows()[0]);
      } catch {
        next = null;
      }
      // A connection that went away, or an answer for somebody no longer signed in.
      if (signal.aborted || (askedFor !== "" && askedFor !== known.current)) return;
      if (next !== null) held.current = next.state === "held" ? { reference: next.reference, reasonsDigest: next.reasonsDigest } : null;
      setReading((prev) => ({ person: known.current, answer: next ?? prev.answer, settled: true }));
    },
    // A new connection, or the stream coming back, is a new question; a recheck
    // while one is out is the same question once more.
    [reachable, connection],
    asks,
  );

  // The render between a change of person and the effect above: nothing of the
  // previous person's is drawn for the next one.
  const current = !enabled
    ? { answer: UNKNOWN, settled: true }
    : reading.person === "" || person === "" || reading.person === person
      ? reading
      : { answer: UNKNOWN, settled: false };

  // Every 30 seconds while held, and never while the document is hidden: the
  // resume signal asks the moment it is shown again.
  const isHeld = current.answer.state === "held";
  useEffect(() => {
    if (!isHeld) return;
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") recheck();
    }, ACCESS_RECHECK_MS);
    return () => window.clearInterval(timer);
  }, [isHeld, recheck]);

  useEffect(() => (enabled ? onAppResume(recheck) : undefined), [enabled, recheck]);

  return { ...current.answer, settled: current.settled, recheck };
}

/** The builtin's one row as an answer, or null when the reply is not one. */
function answerFrom(raw: Row | undefined): DesktopAccessAnswer | null {
  if (raw === undefined || raw === null) return null;
  const row = flatten(raw);
  if (typeof row["admitted"] !== "boolean") return null;
  if (row["admitted"]) return { state: "admitted", exempt: row["exempt"] === true, sources: sourcesFrom(row["sources"]) };
  // Absent when the hold line could not be written: the page then shows no reference.
  return { state: "held", reference: text(row, "reference"), reasonsDigest: text(row, "reasonsDigest") };
}

function sourcesFrom(raw: unknown): AccessSourceRow[] {
  if (!Array.isArray(raw)) return [];
  const out: AccessSourceRow[] = [];
  for (const item of raw) {
    if (item === null || typeof item !== "object" || Array.isArray(item)) continue;
    const entry = item as Row;
    const kind = text(entry, "kind");
    const state = text(entry, "state");
    if (kind === "" || state === "") continue;
    const source: AccessSourceRow = { kind, state, own: entry["own"] === true };
    const lender = text(entry, "lenderUserId");
    const label = text(entry, "label");
    const app = text(entry, "app");
    if (lender !== "") source.lenderUserId = lender;
    if (label !== "") source.label = label;
    if (app !== "") source.app = app;
    out.push(source);
  }
  return out;
}

function text(row: Row, key: string): string {
  const value = row[key];
  return typeof value === "string" ? value.trim() : "";
}
