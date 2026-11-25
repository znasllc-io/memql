import { useCallback, useRef, useState } from "react";
import { isTransportInterruption, refusalSentence, type QueryClient, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useReleaseRead } from "../releases/useReleaseRead";

// MEMORY SEARCH'S READS AND ACTS (design record
// docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md,
// section 8), over the six builtins in component/memql
// memory_search_builtins.go.
//
// Both reads are POLLED, through useReleaseRead: nothing about them is
// broadcast. The status is a row the engine computes per call, and the index
// job's run rows have no routing rule (ruling R3), so a subscription would
// never move. The status is read every 5 s while a switch runs, because its
// progress is what the page is for then, and every 30 s otherwise. The choices
// are read every 30 s even then: a running switch hides them (one switch at a
// time), so a faster read would only spend the caller's catalog. Neither is
// read while the page is hidden or the cluster is away, and a read never
// overlaps the one before it (useReleaseRead).
//
// Every act answers the status after it (memorySearchAct), and the page shows
// that answer at once rather than the state from before the press; it stands
// until the next read lands, which the act asks for. A refusal is the engine's
// own sentence ("changed meanwhile, try again" is a fence the job or another
// owner moved), and the status is read again after it, because the page is
// showing a state that is gone. The refusal stays while reads show the state it
// answered, and goes with that state (changeModelState). No act is repeated:
// not on a timer, not on a reconnect, not after a refusal.

/** The capability resource the page and its builtins are gated on (owner only, by seed). */
export const MEMORY_SEARCH_RESOURCE = "app:cluster/memorysearch";

export type MemorySearchQueries = Pick<QueryClient,
  "memorySearchStatus" | "memorySearchChoices" | "memorySearchSwitch" | "memorySearchCancel" | "memorySearchSwitchAnyway" | "memorySearchConfirm">;

export interface MemorySearchCounts { total: number; done: number; skipped: number; unsourced: number; failed: number }

/** The switch run's summary: `estimatedTotal` is an ESTIMATE (the active index plus its backlog). */
export interface MemorySearchSwitch {
  state: string;
  targetRef: string;
  targetLabel: string;
  counts: MemorySearchCounts;
  startedAt: string;
  startedBy: string;
  waitingReason: string;
  estimatedTotal: number;
  /** The RUN's indexer, whose machines the rebuild embeds on, BARE as the wire carries it. */
  indexedBy: string;
  indexedByName: string;
  /** Choosing the switch's target again would make the caller the run's indexer (ruling R14). */
  takeOver: boolean;
}

export interface MemorySearchStatus {
  /** Empty when no model is bound yet. */
  providerRef: string;
  kind: "machine" | "vendor" | "";
  label: string;
  /** Whose machines index it, BARE as the wire carries it; empty for a vendor model. */
  indexedBy: string;
  indexedByName: string;
  chosenBy: string;
  /** A vendor model nobody chose: the one-time question. */
  needsConfirmation: boolean;
  /** The model the binding replaced: the kept index Switch back names. */
  previousRef: string;
  /** Whether the reader may act at all: an active cluster owner holding the part's execute (the engine's answer). */
  mayAct: boolean;
  /** Choosing the model in use again would make the caller its indexer (ruling R13). */
  takeOver: boolean;
  indexed: number;
  backlog: number;
  backlogCountedAt: string;
  waitingReason: string;
  switch: MemorySearchSwitch | null;
  retained: { providerRef: string; retireAt: string }[];
}

export interface MemorySearchChoice {
  providerRef: string;
  kind: "machine" | "vendor";
  label: string;
  /** The machines offering it for embeddings; 0 for a vendor model. */
  machines: number;
  paidByCluster: boolean;
  /** A machine model with none of its machines online cannot be checked, so cannot be chosen now. */
  available: boolean;
  hint: string;
}

export interface MemorySearch {
  /** Null until the first read lands. */
  status: MemorySearchStatus | null;
  /** Null until the first read lands, or when the page asked for none. */
  choices: MemorySearchChoice[] | null;
  /** The last act's refusal, in the engine's words; "" when the last act went through, or once a read shows another state (changeModelState). */
  error: string;
  /** Why the status could not be read, when the last read failed, in the engine's words (refusalSentence). */
  readError: string;
  /** The same for the choices. */
  choicesError: string;
  retry: () => void;
  /** Each resolves whether the act happened, so a button never says it did when it did not. */
  switchTo: (providerRef: string) => Promise<boolean>;
  cancel: () => Promise<boolean>;
  switchAnyway: () => Promise<boolean>;
  confirm: () => Promise<boolean>;
}

const text = (v: unknown): string => typeof v === "string" ? v.trim() : "";
const whole = (v: unknown): number => typeof v === "number" && Number.isFinite(v) && v > 0 ? Math.floor(v) : 0;
const record = (v: unknown): Record<string, unknown> => v !== null && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : {};

/** A switch in these states is over, and a new one may start (memorySearchSwitchOver). */
export function switchOver(state: string): boolean {
  return state === "done" || state === "cancelled" || state === "failed";
}

/** A switch is in progress: every state but the three that end one. */
export function switchRunning(status: MemorySearchStatus | null): boolean {
  return !!status?.switch && !switchOver(status.switch.state);
}

/**
 * The switch has happened -- the binding names its target -- and only its walk
 * after the flip remains (memorySearchFlipped, ruling R11). The page then reads
 * as after a switch: another model may be chosen, and Cancel is refused.
 */
export function switchFlipped(status: MemorySearchStatus | null): boolean {
  const sw = status?.switch;
  return !!sw && (sw.state === "finishing" || sw.state === "waiting") && sw.targetRef !== "" && sw.targetRef === status!.providerRef;
}

/**
 * WHAT A QUESTION OR A REFUSAL WAS ABOUT: the model in use, and the switch's
 * state and target, which together decide what the page's Change model section
 * shows (the choices or the switch). A refusal, or a confirmation left open,
 * answers the state it was given over. Once a read lands with another one --
 * a switch started, cancelled or flipped elsewhere -- the page drops it rather
 * than keep it for later: a section that swapped to the switch and back hours
 * after would otherwise reopen the old question, or say a refusal about a
 * state long gone (step 6 review).
 */
export function changeModelState(status: MemorySearchStatus | null): string {
  if (status === null) return "";
  return JSON.stringify([status.providerRef, status.switch?.state ?? "", status.switch?.targetRef ?? ""]);
}

export function readMemorySearchStatus(raw: Row | null): MemorySearchStatus {
  const row = record(raw);
  const kind = text(row.kind);
  const sw = row.switch === null || row.switch === undefined ? null : record(row.switch);
  const counts = record(sw?.counts);
  return {
    providerRef: text(row.providerRef),
    kind: kind === "machine" || kind === "vendor" ? kind : "",
    label: text(row.label),
    indexedBy: text(row.indexedBy),
    indexedByName: text(row.indexedByName),
    chosenBy: text(row.chosenBy),
    needsConfirmation: row.needsConfirmation === true,
    previousRef: text(row.previousRef),
    mayAct: row.mayAct === true,
    takeOver: row.takeOver === true,
    indexed: whole(row.indexed),
    backlog: whole(row.backlog),
    backlogCountedAt: text(row.backlogCountedAt),
    waitingReason: text(row.waitingReason),
    switch: sw === null ? null : {
      state: text(sw.state),
      targetRef: text(sw.targetRef),
      targetLabel: text(sw.targetLabel) || text(sw.targetRef),
      counts: { total: whole(counts.total), done: whole(counts.done), skipped: whole(counts.skipped), unsourced: whole(counts.unsourced), failed: whole(counts.failed) },
      startedAt: text(sw.startedAt),
      startedBy: text(sw.startedBy),
      waitingReason: text(sw.waitingReason),
      estimatedTotal: whole(sw.estimatedTotal),
      indexedBy: text(sw.indexedBy),
      indexedByName: text(sw.indexedByName),
      takeOver: sw.takeOver === true,
    },
    retained: (Array.isArray(row.retained) ? row.retained : [])
      .map((entry) => ({ providerRef: text(record(entry).providerRef), retireAt: text(record(entry).retireAt) }))
      .filter((entry) => entry.providerRef !== ""),
  };
}

export function readMemorySearchChoices(rows: Row[]): MemorySearchChoice[] {
  return rows.map((raw) => {
    const row = record(raw);
    return {
      providerRef: text(row.providerRef),
      kind: text(row.kind) === "vendor" ? "vendor" as const : "machine" as const,
      label: text(row.label) || text(row.providerRef),
      machines: whole(row.machines),
      paidByCluster: row.paidByCluster === true,
      available: row.available === true,
      hint: text(row.hint),
    };
  }).filter((choice) => choice.providerRef !== "");
}

type ActName = "memorySearchSwitch" | "memorySearchCancel" | "memorySearchSwitchAnyway" | "memorySearchConfirm";

/**
 * Memory search's status and choices, and the owner's four acts. `connected`
 * and `visible` decide whether anything is read; `choices: false` reads the
 * status alone (Settings' link row).
 */
export function useMemorySearch(query: MemorySearchQueries | null, { visible = true, connected = true, choices = true }: { visible?: boolean; connected?: boolean; choices?: boolean } = {}): MemorySearch {
  const reading = visible && connected;
  const [running, setRunning] = useState(false);
  const status = useReleaseRead("memory-search-status", query ? async (signal) => readMemorySearchStatus((await query.memorySearchStatus({}, { signal, priority: "background" })).single()) : null,
    reading, query, running ? 5_000 : 30_000);
  const choiceRead = useReleaseRead("memory-search-choices", query && choices ? async (signal) => readMemorySearchChoices((await query.memorySearchChoices({}, { signal, priority: "background" })).rows()) : null,
    reading, query, 30_000);

  // WHAT AN ACT ANSWERED, until a read lands. `base` is the read it answered
  // over: a newer read is a newer state, and replaces it.
  const [reply, setReply] = useState<{ value: MemorySearchStatus; base: MemorySearchStatus | null } | null>(null);
  const latestRead = useRef(status.value);
  latestRead.current = status.value;
  const shown = reply !== null && reply.base === status.value ? reply.value : status.value;
  const latestShown = useRef(shown);
  latestShown.current = shown;
  // The cadence follows what is shown, in the same render (React's "information
  // from previous renders" form, guarded so it settles): the read that first
  // sees a switch, or the act that starts one, moves to 5 s at once rather than
  // after the next 30 s read. A changed cadence reads once at once.
  if (switchRunning(shown) !== running) setRunning(switchRunning(shown));

  // The last act's refusal, with the state it answered (changeModelState). The
  // read a refusal asks for usually finds the same state, and the refusal stays
  // beside what was pressed; a read with another state drops it, in the same
  // render form as the cadence above.
  const [refusal, setRefusal] = useState<{ sentence: string; over: string } | null>(null);
  if (refusal !== null && changeModelState(shown) !== refusal.over) setRefusal(null);
  const acting = useRef(false);
  const { retry } = status;
  const act = useCallback(async (name: ActName, run: (q: MemorySearchQueries) => Promise<Result>): Promise<boolean> => {
    if (!query || acting.current) return false;
    acting.current = true;
    setRefusal(null);
    try {
      const answered = (await run(query)).single();
      if (answered) setReply({ value: readMemorySearchStatus(answered), base: latestRead.current });
      return true;
    } catch (e) {
      // A dropped connection is not a refusal: what happened is unknown, and
      // the read that follows says what the cluster holds now -- at once, or,
      // with the cluster away, once it answers, so the sentence promises no
      // read. The state is read as the answer arrived: a read that landed
      // while the act was out is what the refusal answers.
      const sentence = isTransportInterruption(e) ? "The cluster did not answer, so it is not known whether this happened." : refusalSentence(e, name);
      setRefusal({ sentence, over: changeModelState(latestShown.current) });
      return false;
    } finally {
      acting.current = false;
      retry();
    }
  }, [query, retry]);

  return {
    status: shown,
    choices: choices ? choiceRead.value : null,
    error: refusal?.sentence ?? "",
    // In the engine's words, once, for every place that shows them: a first
    // read's notice and a later read's heading note say one thing.
    readError: status.error ? refusalSentence(status.error, "memorySearchStatus") : "",
    choicesError: choices && choiceRead.error ? refusalSentence(choiceRead.error, "memorySearchChoices") : "",
    retry: useCallback(() => { status.retry(); choiceRead.retry(); }, [status.retry, choiceRead.retry]),
    switchTo: useCallback((providerRef: string) => act("memorySearchSwitch", (q) => q.memorySearchSwitch({ provider: providerRef })), [act]),
    cancel: useCallback(() => act("memorySearchCancel", (q) => q.memorySearchCancel({})), [act]),
    switchAnyway: useCallback(() => act("memorySearchSwitchAnyway", (q) => q.memorySearchSwitchAnyway({})), [act]),
    confirm: useCallback(() => act("memorySearchConfirm", (q) => q.memorySearchConfirm({})), [act]),
  };
}
