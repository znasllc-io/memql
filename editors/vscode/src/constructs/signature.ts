// Finding a construct's declaration in a `.memql` file.
//
// WHY THIS EXISTS AT ALL, AND WHY IT IS NOT A PARSER. `ListConstructs` reports
// a file PATH and no range, so an editor that opens the file has nothing
// telling it where in the file to land. Every other jump in this extension
// comes from the language server, which owns all `.memql` parsing -- and the
// rule that nothing else parses construct syntax is the reason the run path is
// trustworthy at all.
//
// So this deliberately does the least thing that is still honest: it looks for
// the line that DECLARES the construct, by its keyword and its name, and
// answers -1 when it cannot find one. It does not tokenize, does not track
// nesting, and makes no claim about the file beyond "this line begins with the
// declaration you asked for".
//
// A MISS OPENS THE FILE AT THE TOP. That is the whole reason the miss is
// reported as -1 rather than as 0: landing on line 1 because the search failed
// and landing on line 1 because the construct is there are different facts,
// and the caller renders them the same way only because both are harmless.
// Landing on the WRONG line would not be -- a developer reading a signature
// that is not the one they asked for has no cue that anything went wrong.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3752 #3747

/**
 * The keyword a kind is AUTHORED under, which is not always the kind the
 * catalog reports.
 *
 * Only `mutation` differs -- it is authored `mutate` -- and the engine keeps
 * the same table for slicing source (`constructKeyword` in
 * component/memql/construct_catalog.go). Written out rather than derived so
 * the one difference is visible.
 */
const KEYWORD_BY_KIND: Readonly<Record<string, string>> = {
  concept: "concept",
  query: "query",
  mutation: "mutate",
  logic: "logic",
  tool: "tool",
  automation: "automation",
  spec: "spec",
  trait: "trait",
  shape: "shape",
  prompt: "prompt",
  provider: "provider",
  builtin: "builtin",
  policy: "policy",
  seed: "seed",
};

/**
 * The zero-based line of `<keyword> [<boundConcept>] <name>`, or -1.
 *
 * The optional middle identifier is the signature binding that
 * query / mutate / shape / spec / seed carry (`query participant
 * spaceParticipants`), so both the two- and three-identifier forms are
 * matched. A concept's catalog name is its canonical id
 * (`v1:cognition:space`) while its declaration carries the bare name, so the
 * last colon-separated segment is what is searched for.
 *
 * Comments are not stripped and strings are not tracked: a line inside a block
 * comment that happens to read exactly like a declaration would match. That is
 * accepted rather than solved -- solving it means parsing, parsing belongs to
 * the language server, and the cost of the miss is landing on a comment about
 * the thing instead of the thing.
 */
export function signatureLine(source: string, kind: string, name: string): number {
  const keyword = KEYWORD_BY_KIND[kind] ?? kind;
  const declared = declaredName(name);
  if (declared === "") return -1;

  // `\b` on both sides so `spaceParticipants` does not match
  // `spaceParticipantsByRole`, and the keyword must open the line so a
  // reference to the construct elsewhere in a body is not mistaken for its
  // declaration.
  const pattern = new RegExp(
    `^\\s*${escapeRegExp(keyword)}\\s+(?:[A-Za-z_][A-Za-z0-9_]*\\s+)?${escapeRegExp(declared)}\\b`,
  );
  const lines = source.split("\n");
  for (let i = 0; i < lines.length; i += 1) {
    if (pattern.test(lines[i]!)) return i;
  }
  return -1;
}

/** A concept's declaration carries the bare name, not its canonical id. */
function declaredName(name: string): string {
  const trimmed = name.trim();
  if (trimmed === "") return "";
  const at = trimmed.lastIndexOf(":");
  return at < 0 ? trimmed : trimmed.slice(at + 1);
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
