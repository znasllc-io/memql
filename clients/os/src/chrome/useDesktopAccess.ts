import { useCallback, useEffect, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../kit/rows";
import { useOsConnection } from "../live/connection";
import { onAppResume } from "../live/resume";
import { useReadEffect } from "../live/useReadEffect";
import { useSession } from "./access";
import { useConnectionRecovery, useConnectionStatus } from "./connection";

// WHETHER THIS PERSON HAS MODEL ACCESS SET UP (design record
// 2026-11-25-who-gets-into-memql-os; the check itself is design record
// 2026-11-24-desktop-access, section 3).
//
// The `desktopAccess` builtin answers for the CALLER only: whether they are a
// cluster owner or developer (exempt), whether they have model access set up
// (machines, apps, keys, the cluster's account, asleep or not), and, when they
// have none, a reference whose log line says what is missing.
//
// WHAT THE ANSWER DECIDES: ASK, AND NOTHING ELSE. A person with none set up
// ("held", the engine's `admitted: false`) enters MemQL OS like anybody and has
// no Ask (`useHasNoModelAccess` in desktopAccessContext.tsx). Release 0.31.41
// kept such a person out of the whole desktop on a page of their own; the
// owner's rule is that the cluster's readiness alone decides who enters
// (CoreGate). The names here and on the wire are from before that.
//
// ===========================================================================
// WHEN IT ASKS AGAIN
// ===========================================================================
// Nothing that gives a person model access reaches their stream: key rows
// publish no graph event, a machine registration is visible only to its owner
// and the cluster's owners, grants are readable from admin rank. So it asks:
//   - on window focus, tab return and network return (`onAppResume`);
//   - when the connection returns (`managed` stream back to "connected");
//   - every 30 seconds while the person has none and the document is visible,
//     so Ask appears by itself soon after an administrator lends a machine or
//     the person adds a key.
// A check asked for while one is out runs once more after it, never beside it
// (`useReadEffect`): two checks in flight for somebody newly without access
// would mint two references and write two lines.
//
// ===========================================================================
// THE REFERENCE TRAVELS WITH THE CLIENT
// ===========================================================================
// A check passes back the reference and digest of the last such answer, so a
// check that finds the same thing writes nothing new (the engine writes a new
// line under the same reference only when the reasons changed): thirty-second
// re-checks add one line to Logs, not one a poll. Nothing lives on a replica,
// so any replica can answer. An admitted answer drops them. They belong to the
// signed-in person, and are dropped when the session names somebody else. The
// engine checks a reference's shape and binds it to the person through the
// digest as well, so this client never hands one person's reference to
// another. NO SURFACE SHOWS THE REFERENCE NOW: the page that did is gone. An
// administrator finds the line in Logs by its component, `desktopAccess`, and
// the person's user id.
//
// ===========================================================================
// WHAT A CHECK THAT FAILS MEANS
// ===========================================================================
// The engine answers a read it could not make with an ERROR, never a row. The
// first check failing is "unknown", which takes nothing away: an outage is not
// evidence that a person lacks access, and the model calls are still refused
// on the server. A later check that fails keeps the answer already held, so a
// dropped call neither gives Ask to a person without access nor takes it from
// one with. A reply with no row is no answer, and reads the same as a failure.
//
// `settled` says whether a check has come back at all, with an answer or a
// failure. The gate's first entry waits on it for up to three seconds from the
// gate's mount, the bound the readiness feed has, so the desk is first drawn
// knowing whether Ask belongs on it. A person whose answer comes later sees
// Ask until it does. A check that fails ends the wait at once.
//
// Regression tests: test/chrome/desktopAccess.test.tsx ("waits at the door for
// the first check" pins the wait); test/system/readinessReactivity.test.tsx
// (Ask in the real shell).

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

/** How often it asks again for a person with no model access, while the tab is visible. */
export const ACCESS_RECHECK_MS = 30_000;

const UNKNOWN: DesktopAccessAnswer = { state: "unknown" };

interface Reading {
  /** The signed-in person the answer is for; "" when it was read before the session named them. */
  person: string;
  answer: DesktopAccessAnswer;
  settled: boolean;
}

export function useDesktopAccess(enabled: boolean): DesktopAccessAnswer & { settled: boolean } {
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

  // Every 30 seconds while the person has no model access, and never while the
  // document is hidden: the resume signal asks the moment it is shown again.
  const isHeld = current.answer.state === "held";
  useEffect(() => {
    if (!isHeld) return;
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") recheck();
    }, ACCESS_RECHECK_MS);
    return () => window.clearInterval(timer);
  }, [isHeld, recheck]);

  useEffect(() => (enabled ? onAppResume(recheck) : undefined), [enabled, recheck]);

  return { ...current.answer, settled: current.settled };
}

/** The builtin's one row as an answer, or null when the reply is not one. */
function answerFrom(raw: Row | undefined): DesktopAccessAnswer | null {
  if (raw === undefined || raw === null) return null;
  const row = flatten(raw);
  if (typeof row["admitted"] !== "boolean") return null;
  if (row["admitted"]) return { state: "admitted", exempt: row["exempt"] === true, sources: sourcesFrom(row["sources"]) };
  // Absent when the engine's line could not be written: nothing to hand back.
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
