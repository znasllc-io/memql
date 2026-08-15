// The local run log: what happened to this machine, one file per run.
//
// `~/.memql/runs/<runId>.json`, rewritten atomically after every step, pruned
// to the 50 most recent on write. History is a directory listing.
//
// THIS IS NOT THE RECEIPT, AND MUST NOT BECOME IT. The install receipt
// (src/install/receipt.ts) answers ONE question -- what is on this machine
// right now -- and it is uninstall's input: its per-artifact pre-existence
// verdicts are what stop an uninstall deleting a developer's own k3d cluster.
// Folding a history into that document makes the question ambiguous, and the
// document that has to be unambiguous is the one with the destructive reader.
// Two artifacts, two questions.
//
// What IS borrowed from the receipt is its discipline, deliberately and in
// full:
//
//  1. PER STEP, ATOMICALLY -- temp file plus rename, so a run killed at any
//     point leaves a record naming exactly the steps that completed, never a
//     truncated file and never one claiming more than was done.
//  2. WRITES ARE SERIALISED PER PATH -- the install executor runs a WAVE of
//     steps concurrently and each one rewrites the whole document, so two
//     unserialised read-modify-writes would drop an item.
//  3. NO SECRET REACHES THE FILE -- anything derived from step params goes
//     through `redactSecrets` on the way in, on the write rather than at the
//     call site, so a param route nobody has written yet is covered too.
//
// Remote runs are NOT written here. They are `v1:cluster:deployment` rows and
// the cluster is their record; mirroring them locally would create a second,
// staler answer to a question the cluster already answers.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3736 #3733

import { randomBytes } from "node:crypto";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { redactSecrets } from "../install/secrets.js";
import {
  runIsTerminal,
  type Run,
  type RunItem,
  type RunItemStatus,
  type RunKind,
  type RunStatus,
} from "./deployments.js";

/** Bumped when the on-disk shape changes incompatibly. */
export const RUN_LOG_VERSION = 1;

/**
 * How many runs are kept.
 *
 * Enough that an operator can see the shape of a week's work, few enough that
 * the directory listing stays a cheap read on every tree refresh. A run is a
 * few kilobytes, so this is a bound on clutter rather than on disk.
 */
export const RUN_LOG_KEEP = 50;

/** The document as it sits on disk: a version stamp around one run. */
interface RunDocument {
  version: number;
  run: Run;
}

/** The installer's own directory, beside the receipt and the tools. */
export function defaultRunsDir(home: string = os.homedir()): string {
  return path.join(home, ".memql", "runs");
}

/**
 * Whether a run id may be used as a filename.
 *
 * A run id reaches the filesystem, so it is checked rather than trusted. The
 * character class is deliberately narrow -- alphanumerics, dash, underscore --
 * which excludes every path separator, `..`, and the leading dot that would
 * make a run invisible to the listing that IS the history.
 */
export function isSafeRunId(id: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(id);
}

export function runFilePath(dir: string, runId: string): string {
  if (!isSafeRunId(runId)) throw new Error(`unsafe run id: ${runId}`);
  return path.join(dir, `${runId}.json`);
}

/**
 * A new run id: sortable stamp, the verb, and enough entropy to not collide.
 *
 * The stamp leads so a directory listing is already in time order before
 * anything parses a file, which is what keeps "the 50 most recent" cheap. The
 * verb is in the name because a half-written run's filename is sometimes all an
 * operator has to go on.
 *
 * `entropy` is injectable so a test can pin the whole id; the default is
 * `crypto.randomBytes` rather than `Math.random` because two runs started in
 * the same millisecond must not land on the same file.
 */
export function mintRunId(kind: RunKind, startedAt: string, entropy?: string): string {
  const stamp = startedAt.replace(/[^0-9A-Za-z]/g, "");
  const suffix = entropy ?? randomBytes(4).toString("hex");
  const id = `${stamp}-${kind}-${suffix}`;
  if (!isSafeRunId(id)) throw new Error(`minted an unsafe run id: ${id}`);
  return id;
}

// ---------------------------------------------------------------------------
// parse
// ---------------------------------------------------------------------------

export type ParseRunResult = { ok: true; run: Run } | { ok: false; error: string };

/**
 * Parses a run record.
 *
 * A record that will not parse is an ERROR to the caller reading one file, and
 * SKIPPED by the caller listing a directory -- see listRuns. Those are the same
 * doctrine the receipt applies to its entries: one bad record must not cost the
 * operator every other run the log still describes correctly.
 *
 * An unreadable ITEM is dropped rather than failing the run, for the same
 * reason. What survives is the header and every item that parsed, which is
 * still a truthful, if shorter, account of what happened.
 */
export function parseRun(text: string): ParseRunResult {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (err) {
    return { ok: false, error: `run record is not JSON: ${(err as Error).message}` };
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ok: false, error: "run record must be a JSON object" };
  }
  const doc = raw as Record<string, unknown>;
  if (doc.version !== RUN_LOG_VERSION) {
    return { ok: false, error: `run record version ${String(doc.version)} is not ${RUN_LOG_VERSION}` };
  }
  const body = doc.run;
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    return { ok: false, error: "run record has no run object" };
  }
  const o = body as Record<string, unknown>;
  const id = str(o.id);
  if (id === "") return { ok: false, error: "run record has no id" };

  const run: Run = {
    id,
    instance: str(o.instance),
    kind: runKind(o.kind),
    startedAt: str(o.startedAt),
    status: runStatus(o.status),
    items: Array.isArray(o.items) ? o.items.map(parseItem).filter(isItem) : [],
  };
  const from = str(o.fromVersion);
  if (from !== "") run.fromVersion = from;
  const to = str(o.toVersion);
  if (to !== "") run.toVersion = to;
  const finished = str(o.finishedAt);
  if (finished !== "") run.finishedAt = finished;
  return { ok: true, run };
}

function parseItem(value: unknown): RunItem | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const o = value as Record<string, unknown>;
  const label = str(o.label);
  if (label === "") return null;
  const item: RunItem = { label, status: itemStatus(o.status) };
  const detail = str(o.detail);
  if (detail !== "") item.detail = detail;
  const at = str(o.at);
  if (at !== "") item.at = at;
  return item;
}

function isItem(value: RunItem | null): value is RunItem {
  return value !== null;
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

const RUN_KINDS = new Set<string>(["install", "upgrade", "repair", "uninstall", "rollout"]);
const RUN_STATUSES = new Set<string>([
  "running",
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
  "superseded",
  "rolled_back",
]);
const ITEM_STATUSES = new Set<string>([
  "pending",
  "running",
  "ok",
  "failed",
  "skipped",
  "preserved",
]);

/**
 * An unrecognised kind reads as `install`.
 *
 * Only reachable from a hand-edited or future-version file, and the header has
 * to say something. `install` is the first verb any machine runs, so it is the
 * least surprising label on a record whose own verb is unreadable.
 */
function runKind(v: unknown): RunKind {
  const value = str(v);
  return RUN_KINDS.has(value) ? (value as RunKind) : "install";
}

/**
 * An unrecognised status reads as `failed`, and this is the ONE place that
 * choice is right.
 *
 * Everywhere else an unknown status means "we cannot tell", and `running` --
 * asserting no outcome -- is the honest reading. Here the record is on OUR
 * disk, written by this extension, and every terminal write goes through
 * `finishRun`. A status this module does not recognise therefore means the file
 * was damaged or truncated mid-life, and a damaged record of a machine-mutating
 * run is not something to draw as still in flight: a spinner that never stops
 * is how a broken record gets ignored rather than looked at.
 */
function runStatus(v: unknown): RunStatus {
  const value = str(v);
  return RUN_STATUSES.has(value) ? (value as RunStatus) : "failed";
}

/** An unreadable item status is `pending`: recorded, outcome unknown. */
function itemStatus(v: unknown): RunItemStatus {
  const value = str(v);
  return ITEM_STATUSES.has(value) ? (value as RunItemStatus) : "pending";
}

export function serializeRun(run: Run): string {
  const doc: RunDocument = { version: RUN_LOG_VERSION, run };
  return `${JSON.stringify(doc, null, 2)}\n`;
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

/** Reads one run, or null when there is no such file. */
export async function readRun(file: string): Promise<Run | null> {
  let text: string;
  try {
    text = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw err;
  }
  const parsed = parseRun(text);
  if (!parsed.ok) throw new Error(`${file}: ${parsed.error}`);
  return parsed.run;
}

/**
 * Every run in the directory, newest first.
 *
 * A file that will not parse is SKIPPED rather than thrown, and a missing
 * directory is an empty history rather than an error: this feeds a tree that
 * has to render, and a machine that has never run an install has no directory.
 * The alternative -- one damaged file blanking the whole view -- would hide the
 * runs that are still perfectly readable, at the moment an operator is most
 * likely looking for them.
 */
export async function listRuns(dir: string): Promise<Run[]> {
  let names: string[];
  try {
    names = await fs.readdir(dir);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw err;
  }
  const runs: Run[] = [];
  for (const name of names) {
    if (!name.endsWith(".json")) continue;
    try {
      const run = await readRun(path.join(dir, name));
      if (run !== null) runs.push(run);
    } catch {
      // Skipped: see above.
    }
  }
  return sortRunsNewestFirst(runs);
}

export function sortRunsNewestFirst(runs: readonly Run[]): Run[] {
  return [...runs].sort((a, b) => {
    if (a.startedAt !== b.startedAt) return a.startedAt > b.startedAt ? -1 : 1;
    return a.id.localeCompare(b.id);
  });
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// One queue per FILE. Two steps of the same wave finishing together each
// rewrite the whole document, and an unserialised pair loses whichever landed
// first -- a lost item is a step the record claims never ran.
const writeQueues = new Map<string, Promise<unknown>>();

function serialise<T>(file: string, work: () => Promise<T>): Promise<T> {
  const prior = writeQueues.get(file) ?? Promise.resolve();
  const next = prior.then(work, work);
  writeQueues.set(
    file,
    next.catch(() => undefined),
  );
  return next;
}

/**
 * Writes a run record, atomically, and prunes the directory.
 *
 * The rename is the point, exactly as it is for the receipt: a rename within a
 * directory is atomic, so any reader -- including the tree refreshing while a
 * run is mid-flight, and the next session after the operator kills this one --
 * sees either the previous record or the new one, never half of either.
 *
 * A FULL REPLACEMENT. Use it to OPEN a record; use `recordRunItem` and
 * `finishRun` to advance one, because those merge against what is on disk.
 */
export async function writeRun(dir: string, run: Run): Promise<void> {
  const file = runFilePath(dir, run.id);
  await serialise(file, async () => {
    await fs.mkdir(dir, { recursive: true });
    await writeAtomic(file, serializeRun(run));
  });
  await pruneRunsDir(dir);
}

/**
 * Read, modify, write -- all three inside the per-file lock.
 *
 * SERIALISING THE WRITES ALONE IS NOT ENOUGH, and this is the whole reason the
 * lock wraps the read as well. The install executor runs a WAVE of steps
 * concurrently; each one holds the run object it was handed and each rewrites
 * the whole document. If the read happens outside the lock, five steps of one
 * wave all compute their update from the same zero-item snapshot, the writes
 * queue neatly, and the last one lands a record naming one step out of five --
 * a record that says four steps never ran, on the artifact whose entire purpose
 * is saying which ones did.
 *
 * ON-DISK ITEMS WIN, THE CALLER'S HEADER WINS. The file is the authority on
 * progress -- it has seen every concurrent sibling -- and the caller is the
 * authority on the header, which only it can change. The same split the
 * receipt's own read-modify-write makes.
 */
async function mutateRun(dir: string, run: Run, mutate: (items: RunItem[]) => RunItem[]): Promise<Run> {
  const file = runFilePath(dir, run.id);
  const next = await serialise(file, async () => {
    await fs.mkdir(dir, { recursive: true });
    let onDisk: Run | null = null;
    try {
      onDisk = await readRun(file);
    } catch {
      // A record that will not parse is REPLACED rather than refused. Unlike the
      // receipt -- which may be the only evidence of a cluster and a CA on this
      // machine, and so is never overwritten -- a run record describes something
      // that already happened and holds nothing an uninstall depends on. Losing
      // a damaged history entry costs less than losing the run in progress.
      onDisk = null;
    }
    const merged: Run = { ...run, items: mutate(onDisk?.items ?? run.items) };
    await writeAtomic(file, serializeRun(merged));
    return merged;
  });
  await pruneRunsDir(dir);
  return next;
}

/** Opens a run's record. */
export async function startRun(dir: string, run: Run): Promise<Run> {
  await writeRun(dir, run);
  return run;
}

/**
 * Records one item and rewrites the record.
 *
 * Returns the MERGED run -- this record plus whatever else landed while the
 * caller was working -- so an in-memory copy converges on the file rather than
 * drifting from it.
 *
 * `params` is the step's invocation flags when the caller has them. They go
 * through `redactSecrets` here, on the WRITE, rather than being trusted from
 * the call site -- the receipt learned that the hard way when an operator
 * pasted an Anthropic key into a path field and it was recorded verbatim into a
 * long-lived file nothing ever rewrites.
 */
export async function recordRunItem(
  dir: string,
  run: Run,
  item: RunItem,
  params?: Record<string, string>,
): Promise<Run> {
  const recorded: RunItem =
    params === undefined ? item : { ...item, detail: describeParams(params, item.detail) };
  return mutateRun(dir, run, (items) => {
    const at = items.findIndex((existing) => existing.label === recorded.label);
    const next = [...items];
    if (at >= 0) next[at] = recorded;
    else next.push(recorded);
    return next;
  });
}

/**
 * A detail line naming the flags a step ran with, with any provider key removed.
 *
 * Prepends whatever detail the caller already had, so a failure's remedy text
 * is not displaced by the parameter list.
 */
export function describeParams(params: Record<string, string>, detail?: string): string {
  const safe = redactSecrets(params);
  const rendered = Object.keys(safe)
    .sort()
    .map((key) => `${key}=${safe[key]}`)
    .join(" ");
  const head = (detail ?? "").trim();
  if (head === "") return rendered;
  if (rendered === "") return head;
  return `${head} · ${rendered}`;
}

/**
 * Closes a run's record at a terminal status.
 *
 * Merges against the file for the same reason `recordRunItem` does: the last
 * step of a wave may still have been landing when the run was declared over,
 * and a close that wrote the caller's snapshot would drop it -- from the record
 * of the run, at the exact moment the record becomes permanent.
 */
export async function finishRun(
  dir: string,
  run: Run,
  status: RunStatus,
  finishedAt: string,
): Promise<Run> {
  return mutateRun(dir, { ...run, status, finishedAt }, (items) => items);
}

/**
 * Closes every run left mid-flight by an extension host that went away.
 *
 * WHY A SWEEP AT ACTIVATION IS THE WHOLE MECHANISM (memql#3886). A local run is
 * driven by this process and its record is rewritten after every step, so the
 * only thing that can leave a file saying `running` is the process dying before
 * it could write the close. Nothing is left to finish that write -- the record
 * stays `running` for as long as the file exists, and `runsToPrune` deliberately
 * never prunes a non-terminal run, so it stays forever and renders as a live
 * spinner for work that ended hours ago.
 *
 * NO PID, NO HEARTBEAT, NO STALENESS WINDOW. A run this host is driving is held
 * in memory by this host, and at activation there are none -- so a non-terminal
 * record on disk at this moment is orphaned BY DEFINITION. Every liveness
 * mechanism that could be built here would be a less certain way of learning
 * something already known, and each would carry its own false positive: a pid
 * reused by an unrelated process, a heartbeat starved by a busy machine, an age
 * threshold that has to guess how long an install is allowed to take.
 *
 * This is why it MUST run before anything else reads the directory, and exactly
 * once per activation. Calling it while a run is in flight would close that
 * run's own record out from under it.
 *
 * Returns the runs it closed, so the caller can say so rather than changing the
 * tree silently.
 */
export async function reconcileOrphanedRuns(dir: string, at: string): Promise<Run[]> {
  const runs = await listRuns(dir);
  const orphaned = runs.filter((run) => !runIsTerminal(run.status));
  const closed: Run[] = [];
  for (const run of orphaned) {
    // ITEM STATUSES ARE LEFT ALONE, and that is the informative choice. The
    // step that says `running` is the step the run died in, which is the single
    // most useful fact this record still holds; rewriting it to `failed` would
    // assert something untrue about a step that may well have completed on the
    // machine, and there is no honest item status for "we stopped watching".
    // Read together -- run `interrupted`, step `running` -- the record says
    // exactly what happened.
    closed.push(await finishRun(dir, run, "interrupted", at));
  }
  return closed;
}

/**
 * Which run files to delete, keeping the `keep` most recent.
 *
 * A RUN THAT IS STILL `running` IS NEVER PRUNED, whatever its age. Pruning is
 * about a directory that has grown, and the one record that must survive a
 * prune is the one currently being written to -- deleting it mid-flight would
 * make the extension's own next write recreate a file with no history in it,
 * losing exactly the steps the operator is watching. A run that never reaches a
 * terminal status (the editor was killed) is kept for the same reason it is
 * worth keeping at all: it is the record of the thing that went wrong.
 *
 * Pure, so the rule is testable without a filesystem.
 */
export function runsToPrune(runs: readonly Run[], keep: number = RUN_LOG_KEEP): Run[] {
  const settled = sortRunsNewestFirst(runs).filter((run) => runIsTerminal(run.status));
  return settled.slice(keep);
}

async function pruneRunsDir(dir: string): Promise<void> {
  let runs: Run[];
  try {
    runs = await listRuns(dir);
  } catch {
    // A listing that fails is not a reason to fail the write that just
    // succeeded. The record is on disk; the directory is merely larger than
    // intended, which the next successful prune corrects.
    return;
  }
  for (const run of runsToPrune(runs)) {
    await fs.rm(runFilePath(dir, run.id), { force: true }).catch(() => undefined);
  }
}

async function writeAtomic(file: string, contents: string): Promise<void> {
  const dir = path.dirname(file);
  await fs.mkdir(dir, { recursive: true });
  const tmp = path.join(
    dir,
    `.${path.basename(file)}.${process.pid}.${randomBytes(4).toString("hex")}.tmp`,
  );
  try {
    await fs.writeFile(tmp, contents, { encoding: "utf8", mode: 0o600 });
    await fs.rename(tmp, file);
  } catch (err) {
    await fs.rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
}
