// The install receipt: what the installer actually put on this machine.
//
// An uninstall graph says which install step each removal reverses. It cannot
// say WHERE the artifact landed (that is a run-time fact -- $HOME/.memql/bin on
// one machine, a --dest override on another), and it cannot say whether the
// installer CREATED the artifact or merely FOUND it. Both live only in the
// result envelope of the step that ran, so both are written down here, per
// step, as the install proceeds.
//
// TWO PROPERTIES ARE LOAD-BEARING.
//
//  1. PER STEP, ATOMICALLY. The receipt is rewritten after EVERY step through
//     a temp file and a rename, so a run killed at any point leaves a receipt
//     describing exactly what happened up to then -- never a truncated file
//     and never a file claiming more than was done. An install that dies
//     half-way is still fully reversible, which is the difference between a
//     bad afternoon and a machine the operator has to clean by hand.
//  2. THE PRE-EXISTENCE VERDICT SURVIVES. remove-artifact.sh refuses
//     unconditionally on --pre-existing=true, and this file is what feeds that
//     flag. A receipt that rounds "was already here" down to false is how an
//     uninstall deletes a developer's own k3d cluster.
//
// Deliberately free of `vscode` imports -- cli.ts runs it under plain node.
//
// Refs: #3372 #3357

import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

/** Bumped when the on-disk shape changes incompatibly. */
export const RECEIPT_VERSION = 1;

/** One executed step, as the uninstall path needs to read it back. */
export interface ReceiptEntry {
  /** The install-graph step id. Uninstall steps name it via `reverses`. */
  stepId: string;
  /** The capability-script id that ran. */
  script: string;
  /** The artifact class left behind, or "" for a step that leaves nothing. */
  receipt: string;
  /** Whether the artifact was already on the machine before the installer ran. */
  preExisting: boolean;
  /** The flags the step was invoked with (graph-pinned plus run-time). */
  params: Record<string, string>;
  /** The script's `result{}` object: what it said about the MACHINE. */
  result: Record<string, unknown>;
  /** The envelope's `changed` flag: whether this run mutated anything. */
  changed: boolean;
  recordedAt: string;
}

export interface Receipt {
  version: number;
  /** The graph that produced it ("install"). */
  graph: string;
  startedAt: string;
  updatedAt: string;
  entries: ReceiptEntry[];
}

export type ParseResult =
  | { ok: true; receipt: Receipt; dropped: number }
  | { ok: false; error: string };

/** The installer's own directory, beside the tools and the checkout it manages. */
export function defaultReceiptPath(home: string = os.homedir()): string {
  return path.join(home, ".memql", "install-receipt.json");
}

export function emptyReceipt(graph: string, now: string = new Date().toISOString()): Receipt {
  return { version: RECEIPT_VERSION, graph, startedAt: now, updatedAt: now, entries: [] };
}

export function serializeReceipt(receipt: Receipt): string {
  return `${JSON.stringify(receipt, null, 2)}\n`;
}

/**
 * Parses a receipt.
 *
 * A receipt that cannot be read is an ERROR, never an empty install: reporting
 * "nothing was installed" is an uninstall that silently leaves a cluster, a CA
 * and three binaries behind. A single unreadable ENTRY is dropped with the rest
 * still usable, because one malformed record must not cost the operator every
 * artifact the receipt still describes correctly.
 */
export function parseReceipt(text: string): ParseResult {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (err) {
    return { ok: false, error: `receipt is not JSON: ${(err as Error).message}` };
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ok: false, error: "receipt must be a JSON object" };
  }
  const obj = raw as Record<string, unknown>;
  if (obj.version !== RECEIPT_VERSION) {
    return { ok: false, error: `receipt version ${String(obj.version)} is not ${RECEIPT_VERSION}` };
  }
  if (!Array.isArray(obj.entries)) {
    return { ok: false, error: "receipt has no entries array" };
  }

  const entries: ReceiptEntry[] = [];
  let dropped = 0;
  for (const e of obj.entries) {
    const entry = parseEntry(e);
    if (entry) entries.push(entry);
    else dropped += 1;
  }
  const now = new Date().toISOString();
  return {
    ok: true,
    dropped,
    receipt: {
      version: RECEIPT_VERSION,
      graph: typeof obj.graph === "string" ? obj.graph : "install",
      startedAt: typeof obj.startedAt === "string" ? obj.startedAt : now,
      updatedAt: typeof obj.updatedAt === "string" ? obj.updatedAt : now,
      entries,
    },
  };
}

function parseEntry(value: unknown): ReceiptEntry | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const o = value as Record<string, unknown>;
  if (typeof o.stepId !== "string" || o.stepId === "") return null;
  if (typeof o.script !== "string" || o.script === "") return null;
  return {
    stepId: o.stepId,
    script: o.script,
    receipt: typeof o.receipt === "string" ? o.receipt : "",
    // An unreadable verdict reads as "already here": see removalParams.
    preExisting: typeof o.preExisting === "boolean" ? o.preExisting : true,
    params: plainStringMap(o.params),
    result: plainObject(o.result),
    changed: o.changed === true,
    recordedAt: typeof o.recordedAt === "string" ? o.recordedAt : "",
  };
}

function plainStringMap(v: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (v === null || typeof v !== "object" || Array.isArray(v)) return out;
  for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
    if (typeof val === "string") out[k] = val;
  }
  return out;
}

function plainObject(v: unknown): Record<string, unknown> {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return {};
  return v as Record<string, unknown>;
}

export function entryFor(receipt: Receipt, stepId: string): ReceiptEntry | undefined {
  return receipt.entries.find((e) => e.stepId === stepId);
}

/** Reads a receipt, or null when one was never written. */
export async function readReceipt(file: string): Promise<Receipt | null> {
  let text: string;
  try {
    text = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw err;
  }
  const parsed = parseReceipt(text);
  if (!parsed.ok) {
    throw new Error(`${file}: ${parsed.error}`);
  }
  return parsed.receipt;
}

// The writes are serialised per path: a wave finishes several steps at once and
// each one rewrites the whole document, so two concurrent read-modify-writes
// would lose an entry -- and a lost entry is a lost artifact.
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
 * Records one executed step, atomically.
 *
 * Read-modify-write of the whole document, then a rename over the target. The
 * rename is the point: a rename within a directory is atomic, so any reader --
 * including the uninstall that runs after the operator kills this one with
 * ^C -- sees either the previous receipt or the new one, never half of either.
 *
 * A receipt that cannot be parsed is REFUSED rather than overwritten: it may be
 * the only record of a cluster and a trust-store CA on this machine.
 */
export async function appendReceiptEntry(file: string, graph: string, entry: ReceiptEntry): Promise<Receipt> {
  return serialise(file, async () => {
    const existing = await readReceipt(file);
    const receipt = existing ?? emptyReceipt(graph);
    receipt.graph = graph;
    receipt.updatedAt = new Date().toISOString();

    const at = receipt.entries.findIndex((e) => e.stepId === entry.stepId);
    if (at >= 0) receipt.entries[at] = entry;
    else receipt.entries.push(entry);

    await writeAtomic(file, serializeReceipt(receipt));
    return receipt;
  });
}

async function writeAtomic(file: string, contents: string): Promise<void> {
  const dir = path.dirname(file);
  await fs.mkdir(dir, { recursive: true });
  const tmp = path.join(dir, `.${path.basename(file)}.${process.pid}.${Math.random().toString(36).slice(2)}.tmp`);
  try {
    await fs.writeFile(tmp, contents, { encoding: "utf8", mode: 0o600 });
    await fs.rename(tmp, file);
  } catch (err) {
    await fs.rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
}

// --------------------------------------------------------------------------
// what uninstall reads back
// --------------------------------------------------------------------------

/**
 * Where each artifact class records its own removal target.
 *
 * An EXPLICIT table, not a heuristic over field names. remove-artifact.sh
 * declares a small set of flags and exits 2 on anything else, so the mapping
 * from "what the install step reported" to "what the removal step needs" is a
 * fact about two named scripts. Writing it out means a reviewer can check it;
 * inferring it means a rename in one script silently stops the other from
 * removing anything.
 */
const REMOVAL_TARGETS: Record<string, { flag: string; resultField: string }> = {
  // install.binary reports the installed file as result.path.
  binary: { flag: "path", resultField: "path" },
  // install.cloneStack reports the checkout directory as result.dest.
  checkout: { flag: "path", resultField: "dest" },
  // install.hostsEntries reports the file it edited as result.hostsFile.
  hostsEntries: { flag: "path", resultField: "hostsFile" },
  // install.mkcert reports the CA directory as result.caroot.
  mkcertCA: { flag: "caroot", resultField: "caroot" },
  // k3d.up reports the cluster it created as result.cluster.
  stack: { flag: "cluster", resultField: "cluster" },
};

/**
 * The flags an uninstall step passes to remove-artifact.sh for this entry, or
 * null when the entry left no artifact behind.
 *
 * `--pre-existing` is always present and always the recorded verdict. That
 * flag is an unconditional refusal inside the script; passing it faithfully is
 * what keeps a developer's own k3d cluster, mkcert CA or checkout when they
 * uninstall memQL. A target the receipt has no recorded value for is OMITTED
 * rather than guessed -- the script exits 2 on a missing required param, and a
 * loud missing --path beats a confident wrong one.
 */
export function removalParams(entry: ReceiptEntry): Record<string, string> | null {
  if (!entry.receipt) return null;
  const params: Record<string, string> = { kind: entry.receipt };
  const target = REMOVAL_TARGETS[entry.receipt];
  if (target) {
    const value = entry.result[target.resultField];
    if (typeof value === "string" && value.trim() !== "") {
      params[target.flag] = value;
    }
  }
  params["pre-existing"] = entry.preExisting ? "true" : "false";
  return params;
}
