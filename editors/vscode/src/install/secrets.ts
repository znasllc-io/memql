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
