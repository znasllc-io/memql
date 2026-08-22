// One format for a `--node` list, for the two paths that disagreed about it
// (memql#4246).
//
// WHAT WENT WRONG. The rebuild screen's hint says "For example: bff, agent" --
// with a space, because that is how a person writes a list -- and the checklist
// tidied the operator's typing for its own sentence. The SEND path forwarded
// the string raw, and `scripts/k3d/dev.sh` splits `--node` on commas only, so
// " agent" reached it as a node type and it exited 2 with "unknown node type".
//
// Two things made that worse than an ordinary refusal. Exit 2's guidance in
// this extension reads "a fault in MemQL rather than in your machine or your
// answers" -- so the failure screen blamed MemQL for the example text MemQL had
// just printed. And the checklist had already stated the list back as
// acceptable, which is the one thing a preflight must never do.
//
// So the rule lives HERE, once, and both paths go through it: the screen words
// what it normalises, and the plan sends what it normalised.
//
// Free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4246

/**
 * An operator's node list as the capability script spells one.
 *
 * Split on the separator the script splits on, trim each entry, drop the empties
 * a trailing or doubled comma leaves, and rejoin with a bare comma. "" out means
 * "no list": the caller passes no flag at all and the script applies its own
 * default -- which is not the same as passing an empty one, and the script
 * treats it as not the same.
 */
export function normalizeNodeList(raw: string): string {
  return raw
    .split(",")
    .map((node) => node.trim())
    .filter((node) => node !== "")
    .join(",");
}
