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

import { redactSecrets, withholdResult } from "./secrets.js";

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
 *   operator already had it and refuse to remove something MemQL created. The
 *   question is "was this here before MemQL ever touched this machine", and
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

    // A SECRET NEVER REACHES THIS FILE (memql#3545, memql#3908). The receipt is
    // long-lived, read back by repair and uninstall for the life of the install,
    // and never rewritten -- so a credential that lands here does not expire and
    // nothing cleans it up. It happened twice, by two different routes:
    //
    //   PARAMS. An operator pasted an Anthropic key into the key-FILE box and
    //   it was recorded verbatim (memql#3545).
    //
    //   RESULTS. `enrolment-link.sh` returns a live `mql_enr_` enrolment URL on
    //   `result.enrolUrl` and `magic-link.sh` returns the owner's single-use
    //   sign-in link on `result.link`, and every step's result was written here
    //   verbatim -- so every install and every repair persisted a plaintext
    //   single-use credential that NOTHING ever read back (memql#3908).
    //
    // Both are cleaned on the WRITE rather than by trusting the caller, and that
    // placement is the point. `state/addCluster.ts` refuses a pasted key where it
    // is typed, which is where a person can be told what to do instead; this
    // covers every other route into the receipt, including ones nobody has
    // written yet -- and for results there IS no other place, because the value
    // comes from a script this code does not own.
    const recorded: ReceiptEntry = {
      ...entry,
      params: redactSecrets(entry.params),
      result: withholdResult(entry.result),
    };

    const at = receipt.entries.findIndex((e) => e.stepId === recorded.stepId);
    if (at >= 0) {
      // `preExisting` RATCHETS: once false, always false (memql#3605).
      //
      // It answers "was this artifact already on the machine before MemQL ever
      // ran", which is a fact about the PAST. A later run cannot change it, and
      // a newest-wins upsert let it flip anyway -- because a repair re-runs
      // `clusterUp` against a cluster that now exists, reports
      // `clusterCreated: false`, and the derivation reads that as "it was
      // already here".
      //
      // The consequence lands at uninstall, which is the one place that cannot
      // afford to be wrong: `refuse_if_pre_existing` is an unconditional exit 3,
      // so after any repair the uninstall REFUSED to delete a cluster MemQL had
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
 *
 * A LIST PER KIND, because one artifact class does not always mean one file
 * (memql#4071). `install.mkcert` writes a CA in one directory AND the front-door
 * pair in another, and the reversal has to be handed both or it takes back half
 * of what the step wrote -- which is exactly what it did, leaving a private key
 * at ~/.memql/certs/dev.key after an uninstall that reported OK on every step.
 */
interface RemovalTarget {
  flag: string;
  resultField: string;
  paramField?: string;
  required: boolean;
}

const REMOVAL_TARGETS: Record<string, RemovalTarget[]> = {
  // install.binary reports the installed file as result.path.
  binary: [{ flag: "path", resultField: "path", required: true }],
  // install.cloneStack reports the checkout directory as result.dest, and is
  // invoked with --dest naming the same directory.
  checkout: [{ flag: "path", resultField: "dest", paramField: "dest", required: true }],
  // install.hostsEntries reports the file it edited as result.hostsFile, and is
  // invoked with --hosts-file. The graph pins neither, so a run that failed
  // before writing leaves nothing to go on -- which is the correct answer,
  // because a block that was never written is not there to remove.
  hostsEntries: [
    {
      flag: "path",
      resultField: "hostsFile",
      paramField: "hosts-file",
      required: true,
    },
  ],
  // install.mkcert reports the CA directory as result.caroot, and the pair it
  // issued as result.certFile / result.keyFile.
  //
  // NOT required, and the difference from `binary` above is worth stating: a
  // missing certFile must not veto the whole removal, because the CA half still
  // has work to do. remove-artifact.sh then leaves any pair alone, which is the
  // right answer for a run that never recorded issuing one. The pair is removed
  // only where MemQL can prove it wrote it -- the marker beside it, not this
  // entry's single `preExisting` verdict, which is a fact about the CA.
  mkcertCA: [
    { flag: "caroot", resultField: "caroot", paramField: "caroot", required: false },
    { flag: "cert-file", resultField: "certFile", paramField: "cert-file", required: false },
    { flag: "key-file", resultField: "keyFile", paramField: "key-file", required: false },
  ],
  // k3d.up reports the cluster it created as result.cluster.
  stack: [{ flag: "cluster", resultField: "cluster", paramField: "cluster", required: false }],
};

/**
 * The flags an uninstall step passes to remove-artifact.sh for this entry, or
 * null when the entry left no artifact behind.
 *
 * `--pre-existing` is always present and always the recorded verdict. That
 * flag is an unconditional refusal inside the script; passing it faithfully is
 * what keeps a developer's own k3d cluster, mkcert CA or checkout when they
 * uninstall MemQL.
 *
 * A REQUIRED TARGET NOBODY RECORDED MEANS THERE IS NOTHING TO REMOVE, and this
 * returns null so the step skips (memql#3564). It used to omit the flag and let
 * remove-artifact.sh exit 2 on the missing param -- reasoning, in a comment,
 * that "a loud missing --path beats a confident wrong one". The first half is
 * right and the conclusion does not follow: there is a third option between
 * guessing and failing, which is recognising that a step which never recorded
 * where it wrote never wrote anywhere. `hostsBlock` failing on a read-only
 * /etc/hosts records `{remedy}` and no `hostsFile`, and the uninstall that
 * followed died on it -- reported to the operator as "a fault in MemQL", which
 * it was, about a hosts block that does not exist.
 *
 * The value is looked for in the result FIRST and in the recorded invocation
 * params SECOND; see REMOVAL_TARGETS for why `binary` has no second chance.
 */
export function removalParams(entry: ReceiptEntry): Record<string, string> | null {
  if (!entry.receipt) return null;
  const params: Record<string, string> = { kind: entry.receipt };
  for (const target of REMOVAL_TARGETS[entry.receipt] ?? []) {
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

/**
 * The COMMIT a previous run actually checked out (memql#3901).
 *
 * WHY A SECOND READER RATHER THAN A WIDER `recordedStackTag`. The two answer
 * different questions and only one of them has an answer on every install:
 *
 *   WHICH RELEASE -- `recordedStackTag`, and it is EMPTY for a branch install
 *   because there is no release. Widening it to return "main" would be worse
 *   than returning nothing: `installPlan` derives the node image tag from it
 *   (`imageTagFor`), and `memql-bff:main` is not a tag anything publishes.
 *
 *   WHICH CODE -- this, and it is set on every path, because clone-stack.sh
 *   resolves whatever it was given to a SHA before checking anything out.
 *
 * That split is what lets `main` be an install choice at all without giving up
 * the property clone-stack.sh's tag-only refusal was protecting: "what is
 * installed on this machine" stays answerable a week later, as a commit for a
 * branch install and as a tag for a tag install -- never as a bare moving ref.
 *
 * Read from the RESULT rather than the params, and that is the point: the params
 * carry what was ASKED for (`--branch=main`, which has moved since), the result
 * carries what was RESOLVED. A repair replays this value, so replaying it must
 * not become an upgrade -- memql#3605's property, which is exactly what a repair
 * from `--branch=main` would have broken.
 */
export function recordedStackCommit(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "stackCheckout");
  const commit = entry?.result?.commit;
  return typeof commit === "string" ? commit.trim() : "";
}

/**
 * What KIND of ref a previous run was asked for: "tag", "branch", "commit", or
 * "" when the receipt predates memql#3901 or records no checkout.
 *
 * The caller needs this to know which question to ask. A repair of a tag install
 * replays the tag (unchanged, and immutable by convention); a repair of a branch
 * install must replay the commit, or "repair" silently means "upgrade to
 * whatever main is today".
 *
 * An OLD receipt returns "" and the caller falls back to the tag, which is
 * correct by construction: every install written before this existed was a tag
 * install, because nothing else was possible.
 */
export function recordedStackRefKind(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "stackCheckout");
  const kind = entry?.result?.refKind;
  return typeof kind === "string" ? kind.trim() : "";
}

/** The directory `install.cloneStack` put the checkout in, or "" when the receipt records none. */
export function recordedStackDir(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "stackCheckout");
  const dest = entry?.result?.dest ?? entry?.params?.dest;
  return typeof dest === "string" ? dest.trim() : "";
}

/**
 * The IMAGE tag a previous run's cluster was actually brought up with (memql#4068).
 *
 * WHY IT IS RECORDED RATHER THAN DERIVED A SECOND TIME. `installPlan` resolves
 * the node-image tag once, from the version the operator chose, and hands it to
 * `clusterUp` as `--image-tag`. Deriving it AGAIN at repair time is the same
 * mistake `recordedCheckout` was written to stop being made about the checkout,
 * and it produced the same silent failure by the same route:
 *
 *   `imageTagForVersion` falls back to DEFAULT_STACK_TAG when it is handed no
 *   version -- and a BRANCH install's `recordedCheckout()` answers with a commit
 *   and an EMPTY tag, deliberately, because there is no release to name. So the
 *   fallback fired on every repair of a branch install: the recorded commit's
 *   manifests reconciled against a DIFFERENT release's engine images. Two harms
 *   at once, and neither announces itself -- an upgrade the operator did not ask
 *   for (which memql#3605 defines a repair as never being) and a manifest/image
 *   skew nobody chose (memql#3602's failure mode, arriving through the button
 *   labelled "repair").
 *
 * READ FROM THE PARAMS, and this is the OPPOSITE choice from
 * `recordedStackCommit` just above -- for the same underlying reason, which is
 * worth stating because the asymmetry looks like an inconsistency. What matters
 * is which side holds the RESOLVED value. For the checkout, the param carries
 * what was ASKED (`--branch=main`, which has moved since) and the result carries
 * what git resolved it to, so the result wins. For the image tag there is
 * nothing to resolve downstream: `installPlan` resolves it BEFORE the flag is
 * built, k3d.up reports no image tag on its envelope at all, and the param is
 * therefore the only record of it -- and already the exact value that ran.
 *
 * WHICH ALSO MEANS THIS WORKS ON RECEIPTS THAT PREDATE THE FIX. `--image-tag`
 * has been passed to `clusterUp` since memql#3572, so any receipt whose install
 * reached that step already carries the answer; nothing had ever read it back.
 * A receipt from an install that failed EARLIER carries nothing, and returns ""
 * -- which is the honest answer, and the caller falls back to deriving.
 *
 * A FAILED `clusterUp` STILL COUNTS. `executor.record` writes an entry for every
 * step that produced an envelope, failed ones included, and the params on it are
 * the ones that run was invoked with. A repair after a cluster that came up
 * half-way should replay the images that half-built cluster was asked for, not
 * whichever release this extension build happens to pin today.
 */
export function recordedImageTag(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "clusterUp");
  const tag = entry?.params?.["image-tag"];
  return typeof tag === "string" ? tag.trim() : "";
}

/**
 * Whether the install this receipt describes BUILT its node images (memql#4430).
 *
 * READ OFF THE `buildImages` ENTRY, which is the only honest evidence: the
 * from-source lane is the lane that ran that step, and a receipt that has an
 * entry for it ran it. Deriving it from the recorded ref kind instead would be
 * wrong for every branch install cut BEFORE this lane existed -- those pulled
 * release images and recorded an image tag, and a repair must keep replaying
 * that tag rather than start looking for `:local` images nothing built.
 *
 * That is the same shape as `recordedImageTag`: the answer is what the run
 * actually did, read back, rather than the same derivation performed twice
 * against inputs that have since changed.
 */
export function recordedImagesFromSource(receipt: Receipt | null): boolean {
  if (!receipt) return false;
  return entryFor(receipt, "buildImages") !== undefined;
}

/**
 * What a repair must replay to reproduce the install a receipt describes: which
 * CODE, and which IMAGES.
 *
 * BOTH IN ONE STRUCT, DELIBERATELY (memql#4068). The two used to be a struct and
 * a separate derivation, and every caller that remembered the first forgot the
 * second -- which is not a coincidence, because the first is the one with the
 * name that sounds like the whole answer. Handing a caller a single object it
 * already destructures is what makes "replayed the checkout, re-derived the
 * images" stop being a state anyone can reach by omission.
 */
export interface RecordedCheckout {
  /** The release tag to replay, or "" when the recorded install was not from one. */
  tag: string;
  /** The commit to replay, or "" when replaying the tag is correct. */
  commit: string;
  /**
   * The node-image tag to replay, or "" when the receipt records none and the
   * caller must derive one. A `clusterUp` fact riding in a struct otherwise
   * about `stackCheckout`, for the reason the interface comment gives.
   */
  imageTag: string;
  /**
   * Whether that install BUILT its images from the checkout (memql#4430).
   *
   * The third thing a repair replays, and it rides here for exactly the reason
   * `imageTag` does: a caller that remembers the ref and forgets the lane hands
   * a cluster running `memql-<node>:local` a GHCR registry and the pin's tag,
   * which is memql#4068's failure with a new cause.
   */
  fromSource: boolean;
  /** Short human label for the run record: the tag, or an abbreviated commit. */
  label: string;
}

/**
 * How to reproduce the checkout a receipt describes (memql#3901).
 *
 * ONE FUNCTION BECAUSE IT IS ONE RULE, and getting it wrong is silent. A repair
 * is defined as "return the cluster to the state its receipt describes" -- it is
 * not an upgrade, and upgrading is a different verb the operator picks by name
 * (memql#3605). Which value reproduces that state depends on what was installed:
 *
 *   TAG INSTALL -> replay the TAG. Unchanged behaviour, and right because a
 *   release tag is immutable by convention, so the tag still names the same
 *   commit. Also what every pre-memql#3901 receipt is, which is why an unknown
 *   refKind falls here rather than erroring: nothing else was possible then.
 *
 *   BRANCH INSTALL -> replay the COMMIT. Replaying `--branch=main` would check
 *   out wherever main is TODAY, so "repair" would silently mean "upgrade" --
 *   memql#3605's exact failure, arriving by the one route that reopens it.
 *
 *   COMMIT INSTALL -> replay the commit, which is trivially the same thing.
 *
 * Returning the two ref fields rather than a discriminated union keeps the
 * caller's job to "pass whichever is non-empty", which is what `installPlan`
 * already does with every other param via `present()`.
 *
 * AND THE IMAGES TRAVEL WITH IT (memql#4068). The rule above decides which CODE
 * a repair replays; it said nothing about which node images that code runs
 * against, and nothing else did either -- the image tag was derived a second
 * time, from a tag a branch install deliberately does not have, and fell through
 * to whatever release the running extension build pinned. That defect arrived with
 * branch installs and only ever affected them (memql#3901): before that every
 * recorded checkout carried a tag, so the second derivation happened to agree
 * with the first. Carrying `imageTag` here rather than leaving it to each caller
 * is the structural half of the fix -- the rule is still one rule, and it is now
 * the whole answer.
 */
export function recordedCheckout(receipt: Receipt | null): RecordedCheckout {
  const tag = recordedStackTag(receipt);
  const commit = recordedStackCommit(receipt);
  const kind = recordedStackRefKind(receipt);
  const imageTag = recordedImageTag(receipt);
  const fromSource = recordedImagesFromSource(receipt);

  if (kind === "branch" || kind === "commit") {
    return {
      tag: "",
      commit,
      imageTag,
      fromSource,
      label: commit === "" ? "" : commit.slice(0, 7),
    };
  }
  // A tag install, or a receipt written before refKind existed. Prefer the tag;
  // fall back to the commit so a receipt that somehow recorded only the latter
  // still reproduces something rather than silently reinstalling the pin.
  if (tag !== "") return { tag, commit: "", imageTag, fromSource, label: tag };
  return {
    tag: "",
    commit,
    imageTag,
    fromSource,
    label: commit === "" ? "" : commit.slice(0, 7),
  };
}

/**
 * The DOMAIN a previous run installed under (memql#3736).
 *
 * Scanned across every entry rather than read off one named step, because the
 * graph pins `--domain` on several of them and which one is present depends on
 * how far the run got. The same scan clusters/presence.ts already makes to
 * choose a probe endpoint -- the difference is only that this returns the
 * domain itself, which is what a surface naming the instance wants.
 *
 * Empty means the receipt records no domain, so the caller's own default
 * applies (`stackPin.DEFAULT_LOCAL_DOMAIN` for a run that took every default).
 */
export function recordedDomain(receipt: Receipt | null): string {
  for (const entry of receipt?.entries ?? []) {
    const domain = entry.params.domain;
    if (typeof domain === "string" && domain.trim() !== "") return domain.trim();
  }
  return "";
}

// ---------------------------------------------------------------------------
// which lane set the running images (memql#4246)
// ---------------------------------------------------------------------------

/**
 * Which lane set the node images that are running right now.
 *
 * `released` is what install, upgrade and repair leave -- `clusterUp
 * --image-tag`, pinned to a published release. `checkout` is what a
 * `k3d.dev` rebuild-from-checkout run leaves -- images built from whatever a
 * developer's own repo-root currently contains, edits included. The two are
 * not a spectrum: a cluster is running one or the other, never both, because
 * the last one to run is what the pods are actually executing.
 */
export type ImageSource = "released" | "checkout";

/** The last rebuild's facts, read back for display. */
export interface RecordedRebuild {
  commit: string;
  ref: string;
  /** Absent when the envelope did not report it -- never defaulted to 0. */
  dirtyCount?: number;
  nodes: string;
  recordedAt: string;
}

/**
 * The last rebuild, when its envelope says the cluster was pointed at
 * checkout images (memql#4246).
 *
 * THE ENVELOPE'S OWN VERDICT, not merely "a rebuild step ran". There is no
 * `--image-source=released` for it to be distinguishing: the script's set is
 * closed to "" or "checkout" and anything else is a bad param, exit 2. What
 * this guard actually rejects is a run whose result does not say `checkout` at
 * all -- a rebuild that FAILED before it patched the Application emits no
 * `imageSource`, and reading that as "the cluster is in checkout mode" would
 * claim a crossing that never happened.
 *
 * A run that patched and then failed DOES say `checkout`, and should: by then
 * the Application is in that lane whatever happened afterwards, which is why
 * dev.sh records the field at the patch rather than at the end of the run.
 */
export function recordedRebuild(receipt: Receipt | null): RecordedRebuild | undefined {
  if (!receipt) return undefined;
  const entry = entryFor(receipt, "rebuildFromCheckout");
  if (entry === undefined || entry.result?.imageSource !== "checkout") return undefined;
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  // `typeof`, not a coercion -- the same rule `rebuiltMessage` states at
  // length. `Number(undefined)` is NaN, which renders "NaN uncommitted"; and
  // `Number(null)` is 0, which renders "0 uncommitted files when it was built"
  // -- a CLAIM that the tree was clean, made from a field never reported.
  const dirty = entry.result.dirtyCount;
  return {
    commit: str(entry.result.commit),
    ref: str(entry.result.ref),
    ...(typeof dirty === "number" && Number.isFinite(dirty) ? { dirtyCount: dirty } : {}),
    nodes: str(entry.result.nodes),
    recordedAt: entry.recordedAt,
  };
}

/**
 * Which lane set the node images last. `released` is what install, upgrade
 * and repair leave (clusterUp --image-tag); `checkout` is what a rebuild
 * leaves. Decided by recordedAt, because both entries survive in the receipt
 * -- a rebuild does not erase the clusterUp entry that preceded it, and a
 * later repair does not erase the rebuild -- so only the ORDER says which
 * one the cluster is actually running.
 *
 * "" means neither lane has ever run: a receipt that predates a clusterUp
 * entry entirely, or none at all. Never guessed at as `released` -- a
 * machine this editor knows nothing about is not evidence that released
 * images are what it runs.
 */
export function recordedImageSource(receipt: Receipt | null): ImageSource | "" {
  if (!receipt) return "";
  const up = entryFor(receipt, "clusterUp");
  const rebuild = recordedRebuild(receipt);
  if (rebuild !== undefined && (up === undefined || rebuild.recordedAt > up.recordedAt)) return "checkout";
  return up === undefined ? "" : "released";
}

/** The cluster owner a previous run bootstrapped. Empty strings when unrecorded. */
export interface RecordedOwner {
  email: string;
  firstName: string;
  lastName: string;
}

/**
 * The OWNER a previous run bootstrapped this cluster with (znasllc-io#3888).
 *
 * WHY A REPAIR NEEDS THIS. `seedBootstrap` refuses a partial bootstrap set --
 * correctly and on purpose, because a partial seed writes a Secret that looks
 * healthy, brings the cluster up green, and leaves the operator at a login page
 * for an account that was never created. But `requiredFields("repair")` does
 * not collect the three owner fields, and `prefillFromReceipt` restored only
 * the provider and its key path, so a repair reached that step with all three
 * empty and died at `exit 2` naming values the wizard had given it no way to
 * supply. The refusal was right; the caller was wrong -- the same shape as
 * memql#3568 and memql#3560 before it.
 *
 * SCANNED ACROSS EVERY ENTRY rather than read off `seedBootstrap` by name, for
 * the reason `recordedDomain` gives just above: which entries carry which
 * params depends on how far the recorded run got, and a repair after a run that
 * failed BEFORE seedBootstrap still has the operator's answers on the entries
 * that did complete. Reading one named step would find nothing in exactly the
 * case a repair is for.
 *
 * The three are read INDEPENDENTLY. A run that recorded two of them and not the
 * third should surface as two pre-filled boxes and one empty one, not as three
 * empty boxes because the set was incomplete -- the operator then retypes what
 * is missing instead of all of it.
 */
export function recordedOwner(receipt: Receipt | null): RecordedOwner {
  const find = (key: string): string => {
    for (const entry of receipt?.entries ?? []) {
      const value = entry.params[key];
      if (typeof value === "string" && value.trim() !== "") return value.trim();
    }
    return "";
  };
  return {
    email: find("owner-email"),
    firstName: find("owner-first-name"),
    lastName: find("owner-last-name"),
  };
}
