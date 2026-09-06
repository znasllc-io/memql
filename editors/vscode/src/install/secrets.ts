// Recognising a provider key that has been typed where a PATH was asked for.
//
// WHY THIS EXISTS. The install collects `providerKeyFile` -- "A PATH to a file
// holding the key, never the key itself", as the field's own hint says -- and
// for a long time that sentence was the entire enforcement. An operator who
// pasted their Anthropic key instead got it accepted, handed to the capability
// script as `--key-file=sk-ant-...` (argv, which `ps` shows to every process on
// the machine) and then written into the install receipt, where it stayed in
// plaintext for the life of the install.
//
// TWO CALLERS, DELIBERATELY. `state/addCluster.ts` refuses the value where it
// is typed, which is the one place a person can be told what to do instead.
// `install/receipt.ts` redacts on the write, which covers every OTHER way a
// param can reach the receipt -- the CLI, a graph-pinned param, a surface
// nobody has written yet. The first is the fix; the second is the wall behind
// it, and a secret at rest is worth a wall.
//
// Deliberately free of `vscode` and of `node:fs`: it is a string predicate, and
// both callers are modules that must stay unit-testable without either.
//
// Refs: #3545 #3544

/**
 * What a redacted param reads as in the receipt.
 *
 * A marker rather than an empty string, and rather than dropping the key
 * outright: `recordedProviderKeyFile` treats "" as "nothing to go on, ask the
 * operator", which is the correct handling for a value that was never there.
 * A value that WAS there and was refused is a different fact, and the operator
 * repairing this install six weeks later deserves to see which one happened.
 */
export const REDACTED = "[redacted: a provider key was given where a path was expected]";

/**
 * Whether a value is a provider API key rather than a path to one.
 *
 * MATCHED ON THE VENDOR PREFIX, not on shape. Both vendors this wizard supports
 * issue keys under `sk-` -- Anthropic as `sk-ant-...`, OpenAI as `sk-...` and
 * `sk-proj-...` -- so one anchored prefix covers the set, and it cannot be
 * fooled by length or entropy heuristics that would also reject a legitimate
 * path with a long random directory name in it.
 *
 * It is not a general secret detector and does not pretend to be. A key from a
 * vendor MemQL does not support is not a value this field can receive, because
 * `provider` is an enum of the two.
 *
 * No absolute path begins `sk-`, and a relative one that did would be refused
 * for a reason its owner can read and work around by giving the absolute form.
 */
export function looksLikeProviderKey(value: string): boolean {
  return /^sk-/i.test(value.trim());
}

/**
 * A copy of `params` with any provider key replaced by REDACTED.
 *
 * Every value is checked, not just the one under `key-file`. Which flag carries
 * the key is a property of the graph document, and a guard that hard-codes the
 * flag name protects exactly today's graph -- the next step to take a
 * credential would be written by someone who had never read this file.
 */
export function redactSecrets(params: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [name, value] of Object.entries(params)) {
    out[name] = looksLikeProviderKey(value) ? REDACTED : value;
  }
  return out;
}

/**
 * The credential prefixes this repository mints, for scanning FREE TEXT.
 *
 * `looksLikeProviderKey` is anchored to a whole value because a param IS the
 * value. A log line is not: a key appears mid-sentence, inside a command echo,
 * or in a `curl -H` a script printed. So this is the same withholding decision
 * applied to a different shape of input, not a second policy.
 */
const LOG_SECRET_RE =
  /\b(sk-[A-Za-z0-9_-]{8,}|mql_(?:pat|wkr|enr|rec)_[A-Za-z0-9_-]{8,})/g;

/**
 * A copy of `text` with anything credential-shaped replaced by REDACTED.
 *
 * WHY THIS EXISTS. The run log now keeps a failing step's own output
 * (memql#5059), which is the first time raw script stderr reaches a file. Step
 * params and results were already withheld; text was not, because none was
 * being written down.
 *
 * It is deliberately a PREFIX scan and not a cleverer one. A regex that tried
 * to recognise secrets by entropy would redact half a docker build log, and the
 * value of keeping the log is that it is readable.
 */
export function redactLogText(text: string): string {
  return text.replace(LOG_SECRET_RE, REDACTED);
}

// ---------------------------------------------------------------------------
// step RESULTS, which are a different problem from step params
// ---------------------------------------------------------------------------
//
// WITHHOLDING GOVERNS THE TWO FILES -- the run log and the receipt -- and
// nothing else. The in-memory ExecutionReport keeps the raw envelope, and a
// value that must reach the OPERATOR'S EYES exactly once does so through a
// named display seam reading that report (install/recoveryKey.ts, the done
// screen's one-time recovery-key reveal), never by weakening these gates.
// That seam exists because of memql#4079: `recoveryKey` was withheld from
// both files, correctly, and no display was ever built, so the step's "show
// it once" was a promise no surface kept -- a redaction gate plus an untested
// promise equals a credential shown to no one. The gates below are unchanged
// by that fix and must stay so.

/**
 * What a withheld result value reads as.
 *
 * Names the field so the record still says the step produced it -- "this step
 * minted a link" is the troubleshooting fact; the link itself is not.
 */
export const WITHHELD = "[withheld: single-use credential]";

/** Result field name words that mean the value is a credential. */
const CREDENTIAL_WORDS = new Set([
  "link",
  "url",
  "uri",
  "token",
  "secret",
  "password",
  "passphrase",
  "credential",
  "code",
  "key",
]);

/**
 * A field name split into its words, lowercased.
 *
 * SPLIT RATHER THAN PATTERN-MATCHED, because the obvious regex is wrong in a
 * way that hides itself. `/(^|[^a-z])key/i` reads as "the word key", and under
 * `i` it matches the `Key` of `apiKey` -- but the character before it is `i`,
 * which IS `[a-z]`, so the guard fails and the whole camelCase half of the
 * namespace slips through: `apiKey`, `enrolUrl`, `authToken`. That defect is
 * invisible in testing because those fields usually ALSO fail the value test,
 * so the field is withheld anyway and by the wrong gate -- until the day one
 * carries a short opaque value, and then nothing catches it.
 *
 * Splitting on both camelCase boundaries and separators makes the rule say what
 * it means, and makes a new word a one-line addition to the set above.
 */
function nameWords(name: string): string[] {
  return name
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .split(/[^A-Za-z0-9]+/)
    .map((word) => word.toLowerCase())
    .filter((word) => word !== "");
}

/**
 * Whether a result VALUE must not be written to the run log.
 *
 * WHY `redactSecrets` IS NOT ENOUGH, AND WHY THIS IS NOT AN EXTENSION OF IT.
 * That function answers a narrow question -- "did an operator paste a provider
 * key into a path field" -- and its own comment is emphatic that it is not a
 * general secret detector. It matches `^sk-` and nothing else. Step RESULTS are
 * a completely different population: `magic-link.sh` returns the owner's
 * single-use sign-in link, `enrolment-link.sh` returns an `mql_enr_` enrolment
 * URL, and both would sail through a `^sk-` test into a file that is written
 * once, kept for fifty runs and never rewritten (memql#3886).
 *
 * SO THIS IS AN ALLOWLIST WEARING A DENYLIST'S CLOTHES. Two independent gates,
 * either of which withholds:
 *
 *  1. The field NAME suggests a credential. Cheap, and it catches the case
 *     where a future script returns something secret in a shape no value test
 *     would flag.
 *  2. The VALUE looks like one -- a URL (which can carry a token in its query
 *     whatever it is called), a known credential prefix, or simply long enough
 *     that nobody is reading it as a status.
 *
 * The bias is deliberate: withholding a harmless value costs an operator one
 * line of detail they can get by re-running the step, and publishing a live
 * credential into a long-lived plaintext file costs them the cluster. Every
 * value this drops is one a script author can restate as a non-secret status
 * field -- which is what `linkState` and `enrolmentState` already are, and why
 * those two survive this filter while `link` and `enrolUrl` do not.
 */
export function withholdsFromRunLog(name: string, value: unknown): boolean {
  if (withholdsFromReceipt(name, value)) return true;
  // THE LENGTH BACKSTOP IS RUN-LOG ONLY, and the asymmetry is the point --
  // see withholdsFromReceipt for the argument. A value nobody would read as a
  // status is worth dropping from a history an operator skims, and is NOT
  // worth dropping from a record the uninstall reads back to find an artifact.
  if (typeof value !== "string") return false;
  const trimmed = value.trim();
  return trimmed.length > 96;
}

/**
 * Whether a result VALUE must not be written to the install RECEIPT.
 *
 * WHY THE RECEIPT NEEDS ITS OWN ANSWER (memql#3908). `~/.memql/install-receipt.json`
 * stores every step's `result` verbatim, and two steps return live credentials:
 * `enrolment-link.sh` puts an `mql_enr_` enrolment URL on `result.enrolUrl`, and
 * `magic-link.sh` puts the owner's single-use sign-in link on `result.link`. So
 * a plaintext single-use credential was written into a long-lived file on every
 * install and every repair. memql#3886 built the withholding machinery and wired
 * it to ONE consumer -- the run log -- and its own comment names `enrolUrl` as a
 * field that must not survive. The receipt is the same argument with a
 * LONGER-LIVED file: written once, kept for the life of the install, never
 * rewritten, and read back by repair and uninstall.
 *
 * And nothing reads either field off the receipt. `enrolmentUrlFrom` read the
 * in-memory ExecutionReport rather than the receipt, and memql#3906 removed even
 * that -- the done screen mints on demand now. A credential persisted for no
 * purpose at all.
 *
 * WHY IT IS NOT SIMPLY `withholdsFromRunLog`. That predicate ALSO drops any
 * string longer than 96 characters, on the reasoning that "nobody is reading it
 * as a status". True of a history an operator skims; false here. The receipt's
 * results are read PROGRAMMATICALLY -- `recordedTarget` pulls
 * `entry.result[target.resultField]` to tell remove-artifact.sh where the
 * artifact is -- and every one of those fields is a filesystem PATH: `path`,
 * `dest`, `hostsFile`, `caroot`. A `--dest` on a deep enough directory crosses
 * 96 characters, and the consequence is not a missing log line. `removalParams`
 * returns null for a required target it cannot find, so the uninstall step SKIPS
 * -- silently leaving a checkout, a CA or a cluster on the machine with nothing
 * that knows it is there. That is memql#3564's failure mode arriving by a new
 * route.
 *
 * So the two gates the run log inherits from here are the ones that identify a
 * credential rather than merely a long string:
 *
 *  1. The field NAME's head noun says it is one (`enrolUrl` is a url, `link` is
 *     a link) -- which is what catches a future script returning a secret in a
 *     shape no value test would flag.
 *  2. The VALUE is credential-SHAPED: an http(s) URL (which carries a token in
 *     its query whatever it is called) or a known credential prefix.
 *
 * The status fields survive both, deliberately and for memql#3886's reason:
 * `enrolmentState`, `linkState` and `ownerClaimed` are the facts a stuck
 * operator needs, and they are states rather than credentials.
 *
 * NESTED VALUES ARE NOT WALKED, matching `redactResult`'s decision and for its
 * reason: a walker is one more place a credential can be reached through a shape
 * nobody anticipated. A nested value whose own key is credential-shaped is still
 * withheld whole by gate 1. No capability script returns a credential inside an
 * object today -- the envelope surface is `cap_result_set` scalars plus a few
 * raw booleans, numbers and string arrays -- and a script author who needs to
 * would be adding the first one.
 */
export function withholdsFromReceipt(name: string, value: unknown): boolean {
  // THE LAST WORD, not any word. In the naming convention these fields actually
  // use, the trailing word is the head noun -- it says what the value IS, and
  // the words before it qualify which one. `enrolUrl` is a url, `apiKey` is a
  // key, `authToken` is a token. But `linkState` is a STATE, and `linkState` is
  // precisely the field that would have told an operator staring at a dead end
  // that the link had been recovered. Withholding it to protect the word "link"
  // would empty the detail view of the one fact it existed to carry.
  //
  // The value gate below remains the backstop for a field whose head noun is
  // innocent and whose value is not.
  const words = nameWords(name);
  const head = words[words.length - 1];
  if (head !== undefined && CREDENTIAL_WORDS.has(head)) return true;
  if (typeof value !== "string") return false;
  const trimmed = value.trim();
  if (trimmed === "") return false;
  if (/^https?:\/\//i.test(trimmed)) return true;
  return /^(sk-|mql_)/i.test(trimmed);
}

// ---------------------------------------------------------------------------
// display redaction -- text on its way to a HUMAN surface (memql#4194)
// ---------------------------------------------------------------------------
//
// The two families above govern the FILES. This third family governs what a
// panel, a tooltip or an output channel may show: capability stderr, exception
// messages and file paths, which routinely carry the operator's home directory
// and can carry a credential a subprocess echoed. One module on purpose -- the
// audit's rule is that there is exactly one redactor, extended here rather
// than duplicated somewhere a future surface would miss.

/** What a credential scrubbed out of display text reads as. */
export const SCRUBBED = "[scrubbed credential]";

/**
 * Replaces the home directory prefix with `~` wherever it appears.
 *
 * A path under $HOME names the operator's account to anyone reading a
 * screenshot or a screen share; `~/.memql/clusters.yaml` says everything the
 * surface needs. Callers pass `os.homedir()`; a degenerate home ("" or "/")
 * masks nothing rather than corrupting every path.
 */
export function maskHomePath(text: string, home: string): string {
  const trimmedHome = home.replace(/[\\/]+$/, "");
  if (trimmedHome.length < 2) return text;
  return text.split(trimmedHome).join("~");
}

/**
 * Text as a human surface may show it: home masked, credentials scrubbed.
 *
 * The credential patterns are the families this product mints, collects or
 * carries -- provider keys (`sk-...`), MemQL credentials (`mql_pat_`,
 * `mql_wkr_`, `mql_rec_`, `mql_enr_`, and any future `mql_*_` sibling), and
 * since memql#4625 the two shapes a SESSION travels as: a JWT and a bearer
 * header.
 *
 * THE JWT AND BEARER RULES ARE DEFENCE IN DEPTH, NOT A RESPONSE TO A LEAK.
 * The #4625 audit could find no path that puts a session token into an error
 * message or an output channel, so the protection today is "nothing puts one
 * there" rather than "anything that does gets stripped". The first is a
 * property of every current call site; the second is a property of this
 * function, and only the second survives somebody adding a call site. The
 * rules cost two regexes.
 *
 * This is still NOT a general secret detector, for `looksLikeProviderKey`'s
 * reason: a heuristic wide enough to catch everything also eats the ordinary
 * paths and ids an operator is reading the text FOR. Both additions are
 * anchored on structure a normal word cannot have -- `eyJ` followed by
 * base64url and a dot is a JWT header segment, and `Bearer ` is a scheme name
 * that only ever precedes a credential.
 */
export function redactForDisplay(text: string, home: string): string {
  return maskHomePath(text, home)
    .replace(/\bsk-[A-Za-z0-9_-]{8,}/g, SCRUBBED)
    .replace(/\bmql_[a-z]{3}_[A-Za-z0-9_-]{8,}/g, SCRUBBED)
    // A JWT: the header segment and everything joined to it. Matching from
    // `eyJ` through the remaining segments means a partially-logged token is
    // scrubbed too -- a truncated JWT is still a credential's prefix.
    .replace(/\beyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_-]*){0,2}/g, SCRUBBED)
    // An Authorization header as anything would print it. The scheme is kept
    // so the line still reads as an auth header rather than becoming a
    // mystery, which is what makes a redacted log worth reading.
    .replace(/\bBearer\s+\S+/gi, `Bearer ${SCRUBBED}`);
}

/**
 * A step's result as the run log may keep it.
 *
 * Scalars only. A nested object or array is summarised rather than walked --
 * the run log is a history an operator skims, not a transcript, and a walker
 * would be one more place a credential could be reached through a shape nobody
 * anticipated.
 */
export function redactResult(result: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [name, value] of Object.entries(result)) {
    if (withholdsFromRunLog(name, value)) {
      out[name] = WITHHELD;
      continue;
    }
    if (value === null || value === undefined) continue;
    if (typeof value === "object") {
      out[name] = Array.isArray(value) ? `[${value.length} items]` : "{...}";
      continue;
    }
    out[name] = String(value);
  }
  return out;
}

/**
 * A step's result as the install RECEIPT may keep it (memql#3908).
 *
 * The sibling of `redactResult`, and it exists as a sibling rather than a reuse
 * because the two callers disagree about one thing that matters: TYPES.
 *
 * `redactResult` returns `Record<string, string>`. It stringifies for a log a
 * person reads, which is right there and wrong here -- `receipt.ts:433` pulls
 * `entry.result[target.resultField]` and every other reader is programmatic too,
 * so stringifying would turn `ownerClaimed: true` into `"true"` and quietly
 * change what every strict reader sees. Passing the receipt through the run
 * log's function was therefore never a one-line option; that is the whole reason
 * memql#3886's machinery reached only one of its two consumers.
 *
 * So this withholds the flagged fields and leaves everything else EXACTLY as the
 * script emitted it: booleans stay booleans, numbers stay numbers, arrays and
 * objects pass through by reference-free copy of the top level. A withheld field
 * is replaced by the WITHHELD marker rather than dropped, for `redactSecrets`'
 * reason -- "this step minted a link" is a troubleshooting fact worth keeping
 * even when the link itself is not.
 *
 * `null` and `undefined` are KEPT rather than skipped, which is the other
 * divergence from the run log: `enrolment-link.sh` reports `enrolUrl: ""` on the
 * awaiting-first-sign-in path, and a reader distinguishing "the step reported
 * nothing" from "the step reported an empty value" should keep being able to.
 */
export function withholdResult(result: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, value] of Object.entries(result)) {
    out[name] = withholdsFromReceipt(name, value) ? WITHHELD : value;
  }
  return out;
}
