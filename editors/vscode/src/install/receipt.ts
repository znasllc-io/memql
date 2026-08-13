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

import { redactSecrets } from "./secrets.js";

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

/**
 * What the receipt knows about one install step, across every run of it.
 *
 * THE RECEIPT IS APPEND-ONLY AND STEPS GET RETRIED, so a step usually has more
 * than one entry: the attempt that failed, then the one that worked. This used
 * to be `entries.find(...)` -- the FIRST, which on any retried step is the
 * OLDEST, which is the failure. An uninstall therefore read the attempt that
 * left nothing behind and never saw the one that did (memql#3564).
 *
 * That is not a case of picking the other end either, because the two questions
 * the uninstall asks have opposite answers in time:
 *
 *   WHERE IS IT -- the newest recorded value. A failed attempt records no
 *   target at all (`hostsBlock` failing before it writes leaves a result of
 *   just `{remedy}`), so the answer has to come from whichever run got far
 *   enough to report one.
 *
 *   IS IT OURS TO REMOVE -- the OLDEST answer, and only the oldest. If the
 *   first run created the mkcert CA (`preExisting: false`) then a later run
 *   found it there (`preExisting: true`), taking the newest would conclude the
 *   operator already had it and refuse to remove something memQL created. The
 *   question is "was this here before memQL ever touched this machine", and
 *   only the first run can answer it.
 *
 * Results and params are merged oldest-to-newest so a later run overrides a key
 * it re-reported and keys it did not report survive.
 */
export function entryFor(receipt: Receipt, stepId: string): ReceiptEntry | undefined {
  const runs = receipt.entries.filter((e) => e.stepId === stepId);
  if (runs.length === 0) return undefined;
  const oldest = runs[0]!;
  const newest = runs[runs.length - 1]!;
  if (runs.length === 1) return newest;
  return {
    ...newest,
    preExisting: oldest.preExisting,
    result: Object.assign({}, ...runs.map((e) => e.result)) as Record<string, unknown>,
    params: Object.assign({}, ...runs.map((e) => e.params)) as Record<string, string>,
  };
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

    // A SECRET NEVER REACHES THIS FILE (memql#3545). The receipt is long-lived,
    // read back by repair and uninstall for the life of the install, and never
    // rewritten -- so a credential that lands here does not expire and nothing
    // cleans it up. It happened: an operator pasted an Anthropic key into the
    // key-FILE box and it was recorded verbatim.
    //
    // Redacting on the WRITE rather than trusting the caller is the point.
    // `state/addCluster.ts` refuses the value where it is typed, which is where
    // a person can be told what to do instead; this covers every other route a
    // param has into the receipt, including ones nobody has written yet.
    const recorded: ReceiptEntry = { ...entry, params: redactSecrets(entry.params) };

    const at = receipt.entries.findIndex((e) => e.stepId === recorded.stepId);
    if (at >= 0) {
      // `preExisting` RATCHETS: once false, always false (memql#3605).
      //
      // It answers "was this artifact already on the machine before memQL ever
      // ran", which is a fact about the PAST. A later run cannot change it, and
      // a newest-wins upsert let it flip anyway -- because a repair re-runs
      // `clusterUp` against a cluster that now exists, reports
      // `clusterCreated: false`, and the derivation reads that as "it was
      // already here".
      //
      // The consequence lands at uninstall, which is the one place that cannot
      // afford to be wrong: `refuse_if_pre_existing` is an unconditional exit 3,
      // so after any repair the uninstall REFUSED to delete a cluster memQL had
      // created -- leaving the operator to run `k3d cluster delete` by hand,
      // which is exactly the manual step the installer exists to remove.
      //
      // Carrying the recorded value forward is the fix, and it fails in the safe
      // direction: an artifact ever seen as pre-existing stays protected.
      const previous = receipt.entries[at];
      if (previous.preExisting === false) recorded.preExisting = false;
      receipt.entries[at] = recorded;
    } else {
      receipt.entries.push(recorded);
    }

    await writeAtomic(file, serializeReceipt(receipt));
    return receipt;
  });
}

/**
 * Removes the receipt, because the install it describes is gone (memql#3544).
 *
 * NOTHING USED TO DO THIS. Not the uninstall that had just removed every
 * artifact the receipt names, not anything else -- so a machine that had been
 * fully and successfully uninstalled still carried the record of an install.
 * `detectPresence` reads that record, so the verdict stayed `installed-*`
 * forever: the Install card was never offered again, Repair re-ran a graph over
 * a cluster that was not there, and Uninstall correctly reported nothing left
 * to remove. There was no control anywhere in the extension that could clear
 * it.
 *
 * IDEMPOTENT, deliberately. The caller is a completion path that may run twice
 * -- an uninstall re-run after a successful one, a retry after a follow-up step
 * failed -- and "the file this asks me to delete is already gone" is the
 * outcome it wanted, not an error to report to an operator.
 *
 * Only ever called after a removal that reported `ok`. A partial uninstall
 * keeps its receipt: it still names artifacts that are still on the machine,
 * and deleting it would strand them with nothing that knows they are there.
 */
export async function deleteReceipt(file: string): Promise<void> {
  await fs.rm(file, { force: true });
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
/**
 * Where each artifact class's removal target comes from.
 *
 * `resultField` is what the install step REPORTED. `paramField` is the flag it
 * was INVOKED with, used when the result carries nothing -- which is what a
 * step that failed part-way records. That is not a guess: it is the exact value
 * the installer used, so if anything landed, it landed there.
 *
 * `binary` deliberately has NO param fallback. install.binary takes
 * `--dest=<directory>` and reports `result.path` as the FILE inside it, so
 * falling back would hand remove-artifact.sh a directory where it expects a
 * file -- and `rm -f` on the wrong noun is not a mistake worth risking to save
 * an edge case that cannot happen anyway (a download that fails leaves a temp
 * file, never the final path).
 *
 * `required` says the script exits 2 without the flag. Where it is false the
 * script has its own default and omitting the flag is fine.
 */
const REMOVAL_TARGETS: Record<
  string,
  { flag: string; resultField: string; paramField?: string; required: boolean }
> = {
  // install.binary reports the installed file as result.path.
  binary: { flag: "path", resultField: "path", required: true },
  // install.cloneStack reports the checkout directory as result.dest, and is
  // invoked with --dest naming the same directory.
  checkout: { flag: "path", resultField: "dest", paramField: "dest", required: true },
  // install.hostsEntries reports the file it edited as result.hostsFile, and is
  // invoked with --hosts-file. The graph pins neither, so a run that failed
  // before writing leaves nothing to go on -- which is the correct answer,
  // because a block that was never written is not there to remove.
  hostsEntries: {
    flag: "path",
    resultField: "hostsFile",
    paramField: "hosts-file",
    required: true,
  },
  // install.mkcert reports the CA directory as result.caroot.
  mkcertCA: { flag: "caroot", resultField: "caroot", paramField: "caroot", required: false },
  // k3d.up reports the cluster it created as result.cluster.
  stack: { flag: "cluster", resultField: "cluster", paramField: "cluster", required: false },
};

/**
 * The flags an uninstall step passes to remove-artifact.sh for this entry, or
 * null when the entry left no artifact behind.
 *
 * `--pre-existing` is always present and always the recorded verdict. That
 * flag is an unconditional refusal inside the script; passing it faithfully is
 * what keeps a developer's own k3d cluster, mkcert CA or checkout when they
 * uninstall memQL.
 *
 * A REQUIRED TARGET NOBODY RECORDED MEANS THERE IS NOTHING TO REMOVE, and this
 * returns null so the step skips (memql#3564). It used to omit the flag and let
 * remove-artifact.sh exit 2 on the missing param -- reasoning, in a comment,
 * that "a loud missing --path beats a confident wrong one". The first half is
 * right and the conclusion does not follow: there is a third option between
 * guessing and failing, which is recognising that a step which never recorded
 * where it wrote never wrote anywhere. `hostsBlock` failing on a read-only
 * /etc/hosts records `{remedy}` and no `hostsFile`, and the uninstall that
 * followed died on it -- reported to the operator as "a fault in memQL", which
 * it was, about a hosts block that does not exist.
 *
 * The value is looked for in the result FIRST and in the recorded invocation
 * params SECOND; see REMOVAL_TARGETS for why `binary` has no second chance.
 */
export function removalParams(entry: ReceiptEntry): Record<string, string> | null {
  if (!entry.receipt) return null;
  const params: Record<string, string> = { kind: entry.receipt };
  const target = REMOVAL_TARGETS[entry.receipt];
  if (target) {
    const value = recordedTarget(entry, target);
    if (value !== "") {
      params[target.flag] = value;
    } else if (target.required) {
      return null;
    }
  }
  params["pre-existing"] = entry.preExisting ? "true" : "false";
  return params;
}

/** The artifact's location as the receipt recorded it, or "" if it did not. */
function recordedTarget(
  entry: ReceiptEntry,
  target: { resultField: string; paramField?: string },
): string {
  const reported = entry.result[target.resultField];
  if (typeof reported === "string" && reported.trim() !== "") return reported;
  if (target.paramField === undefined) return "";
  const invoked = entry.params[target.paramField];
  return typeof invoked === "string" && invoked.trim() !== "" ? invoked : "";
}

/**
 * The provider-key PATH a previous run recorded, if there is one (memql#3512).
 *
 * WHY THIS EXISTS. memql#3473 made `providerKey` a gate: every mutating step
 * declares `dependsOn: [..., providerKey]`, so nothing runs until the key
 * verifies. A REPAIR collects only the domain -- deliberately, since a repair
 * re-runs a graph over a machine that already recorded these answers -- which
 * left the step with no `--key-file` and failed it with exit 2 before anything
 * else could start. Every repair, every time.
 *
 * The answer is the receipt, which is precisely the record of what the install
 * did. `executor.ts` writes an entry for every step that returns an envelope,
 * `params` included, and `providerKey` returns one even though it is
 * `readOnly` and leaves no artifact -- so the path the operator gave the
 * install is on disk, and a repair can use it without asking again.
 *
 * Returns "" when there is nothing to go on: no receipt, no `providerKey`
 * entry, or an entry that recorded no `key-file`. The caller must treat that
 * as "ask" rather than as "proceed" -- starting a run that cannot pass wave 2
 * is the bug this exists to fix.
 */
export function recordedProviderKeyFile(receipt: Receipt | null): string {
  return recordedProviderParam(receipt, "key-file");
}

/**
 * The VENDOR a previous run verified its key against (memql#3473).
 *
 * Travels with the path above, and has to: the wizard now collects the provider
 * rather than pinning `anthropic`, so a repair that read the key file back and
 * re-asserted the DEFAULT vendor would verify an OpenAI key against Anthropic's
 * API. That is an exit 3 -- REFUSED, "the provider rejected the key" -- about a
 * key that is perfectly good, which is the most misleading answer available.
 *
 * Empty means the same thing it means above: nothing to go on, so ask.
 */
export function recordedProvider(receipt: Receipt | null): string {
  return recordedProviderParam(receipt, "provider");
}

function recordedProviderParam(receipt: Receipt | null, name: string): string {
  if (receipt === null) return "";
  const entry = entryFor(receipt, "providerKey");
  const recorded = entry?.params[name];
  return typeof recorded === "string" ? recorded : "";
}

/**
 * The release tag a previous run CHECKED OUT (memql#3605).
 *
 * Travels with the provider and the key path above, and for a sharper reason: a
 * repair that supplied no tag fell through to `DEFAULT_STACK_TAG`, so repairing
 * a v0.16.1 cluster from a v0.17.0 extension SILENTLY UPGRADED it -- new
 * manifests reconciled over old binaries, which is exactly the skew memql#3602
 * was opened about, arriving this time by way of a button labelled "repair".
 *
 * A repair returns a cluster to the state its receipt describes. It is not an
 * upgrade, and it must not become one by omission; upgrading is a different
 * verb, which the operator picks by name.
 *
 * Empty means the receipt records no checkout, so the caller's default applies.
 */
export function recordedStackTag(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "stackCheckout");
  const tag = entry?.params?.tag;
  return typeof tag === "string" ? tag.trim() : "";
}
