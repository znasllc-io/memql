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
 * vendor memQL does not support is not a value this field can receive, because
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

// ---------------------------------------------------------------------------
// step RESULTS, which are a different problem from step params
// ---------------------------------------------------------------------------

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
  if (/^(sk-|mql_)/i.test(trimmed)) return true;
  return trimmed.length > 96;
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
