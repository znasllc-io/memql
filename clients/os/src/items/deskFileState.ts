// What a desk file icon says, with no dot (José, Nov 19 call: "quita ese
// indicator"; Jesús picked the states on Nov 20 2026). A file that cannot open
// yet -- still uploading, or made on a computer that is offline -- is drawn a
// dimmer grey and can still be picked, moved, filed into a folder or thrown in
// the Bin; it only does not open. Opening it anyway waits: the file opens by
// itself the moment it can, with nothing announcing it. A failed upload drops
// whatever was waiting. An upload that finishes with nobody waiting says
// "Ready" for a moment, then the icon is plain.
//
// The rules are pure functions over each icon's HOLD (what stops it opening),
// so they are tested without a browser; `useDeskFileStates` only runs them as
// the holds change and keeps the Ready timers.

import { useEffect, useReducer, useRef } from "react";

import { deriveProvenance, type MachinePresence } from "./provenance";
import type { FileEntry } from "../system/desktop";

/** How long a finished upload nobody tried to open says "Ready". */
export const READY_MS = 3000;

/** What stops a desk file opening right now; null when nothing does. */
export type Hold = "uploading" | "failed" | "offline" | null;

/** What a file was asked to do while held: open, or go into a folder. */
export type Wait = "open" | { folderId: string };

/** The one line under a desk file's name, where the retry line goes. */
export type DeskLine = "opening" | "failed" | "offline" | "ready" | null;

export interface DeskOpens {
  /** Files asked to do something they could not do yet, by item id. */
  waiting: ReadonlyMap<string, Wait>;
  /** Files whose upload just finished with nothing waiting: "Ready", briefly. */
  ready: ReadonlySet<string>;
}

export const NO_OPENS: DeskOpens = { waiting: new Map(), ready: new Set() };

/** "Offline" is the provenance rule's own unreachable: the machine that made
 *  the file is known and offline. A machine nobody has heard of yet holds
 *  nothing back, exactly as it drew no amber dot. */
export function holdOf(
  file: Pick<FileEntry, "source" | "producedByWorkerId" | "uploadState">,
  machine: MachinePresence | null,
): Hold {
  if (file.uploadState) return file.uploadState;
  return deriveProvenance(file, machine).tone === "unreachable" ? "offline" : null;
}

// Opening needs the bytes, so it waits for both the upload and the computer;
// filing into a folder is a Library write, so it waits only for the upload.
function releases(wait: Wait, hold: Hold): boolean {
  return wait === "open" ? hold === null : hold === null || hold === "offline";
}

/** Somebody asked `id` to open (or go into a folder) while held by `hold`. */
export function ask(opens: DeskOpens, id: string, hold: Hold, wait: Wait): { opens: DeskOpens; now: boolean } {
  // A failed upload offers its retry line instead; nothing waits on it.
  if (hold === "failed") return { opens, now: false };
  if (releases(wait, hold)) return { opens, now: true };
  return { opens: { ...opens, waiting: new Map(opens.waiting).set(id, wait) }, now: false };
}

/**
 * The icons' holds moved from `before` to `after`. Returns what waited and may
 * go now (`release`), and which files just finished uploading with nothing
 * waiting (`readied`, to clear after READY_MS). A file that failed, or left
 * the desk, drops what it was waiting for.
 */
export function settle(
  opens: DeskOpens,
  before: ReadonlyMap<string, Hold>,
  after: ReadonlyMap<string, Hold>,
): { opens: DeskOpens; release: Array<[string, Wait]>; readied: string[] } {
  const waiting = new Map(opens.waiting);
  const ready = new Set(opens.ready);
  const release: Array<[string, Wait]> = [];
  const readied: string[] = [];
  for (const [id, wait] of opens.waiting) {
    const hold = after.get(id);
    if (hold === undefined || hold === "failed") waiting.delete(id);
    else if (releases(wait, hold)) {
      waiting.delete(id);
      release.push([id, wait]);
    }
  }
  for (const id of opens.ready) if (after.get(id) !== null) ready.delete(id);
  for (const [id, hold] of after) {
    if (hold === null && before.get(id) === "uploading" && !opens.waiting.has(id)) {
      ready.add(id);
      readied.push(id);
    }
  }
  const changed = release.length > 0 || readied.length > 0 || waiting.size !== opens.waiting.size || ready.size !== opens.ready.size;
  return { opens: changed ? { waiting, ready } : opens, release, readied };
}

/** A Ready line's moment is over. */
export function unready(opens: DeskOpens, id: string): DeskOpens {
  if (!opens.ready.has(id)) return opens;
  const ready = new Set(opens.ready);
  ready.delete(id);
  return { ...opens, ready };
}

export function lineOf(hold: Hold, wait: Wait | undefined, ready: boolean): DeskLine {
  if (hold === "failed") return "failed";
  if (hold === "offline") return "offline";
  if (hold === "uploading") return wait === "open" ? "opening" : null;
  return ready ? "ready" : null;
}

/**
 * The desk's file states, run over every file icon's hold. `release` does what
 * waited (opens the file, or files it into the folder) once it can.
 */
export function useDeskFileStates(holds: ReadonlyMap<string, Hold>, release: (id: string, wait: Wait) => void) {
  const opens = useRef<DeskOpens>(NO_OPENS);
  const before = useRef(holds);
  const releaseNow = useRef(release);
  releaseNow.current = release;
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const [, rerender] = useReducer((n: number) => n + 1, 0);
  const commit = (next: DeskOpens) => {
    if (next === opens.current) return;
    opens.current = next;
    rerender();
  };

  useEffect(() => {
    const step = settle(opens.current, before.current, holds);
    before.current = holds;
    commit(step.opens);
    for (const [id, wait] of step.release) releaseNow.current(id, wait);
    for (const id of step.readied) {
      const timer = setTimeout(() => {
        timers.current.delete(timer);
        commit(unready(opens.current, id));
      }, READY_MS);
      timers.current.add(timer);
    }
    // `commit` only touches refs and the reducer's stable dispatch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [holds]);

  useEffect(() => {
    const pending = timers.current;
    return () => pending.forEach(clearTimeout);
  }, []);

  return {
    line: (id: string): DeskLine => lineOf(holds.get(id) ?? null, opens.current.waiting.get(id), opens.current.ready.has(id)),
    ask: (id: string, wait: Wait) => {
      const step = ask(opens.current, id, holds.get(id) ?? null, wait);
      commit(step.opens);
      if (step.now) releaseNow.current(id, wait);
    },
  };
}
