// Where the install graph and its capability scripts actually are, at run time.
//
// WHY THIS EXISTS. SessionOptions.root is the directory `graph.ts` and
// `runner.ts` both hard-code paths beneath -- `<root>/scripts/install/graph/
// <kind>.json` and `<root>/<CAPABILITY_SCRIPTS[id]>`. In a checkout that is the
// repository root and everything is already there. In an INSTALLED extension it
// is nothing: a .vsix contains only files from under the extension directory, so
// a sibling `scripts/` tree at the repository root is not in the archive and not
// on the machine. cli.ts's `path.resolve(__dirname, "..", "..", "..")` is correct
// from a checkout and meaningless from a .vsix, and until this module there was
// no other answer -- which is why nothing could start an install from the panel
// (memql#3487).
//
// THE LAYOUT IS REPRODUCED, NOT FLATTENED. Packaging stages the tree into
// `<extension>/staged/scripts/...`, preserving every path segment, and this
// returns `<extension>/staged`. The resolvers are not taught a second shape and
// do not take a second path. That is deliberate: two roots that may disagree in
// SHAPE is exactly the seam a "worked from the terminal, not from the editor"
// bug grows in, and the whole point of session.ts is that there is one run path.
// Packaged and checkout differ only in WHERE the root is, never in what is under
// it.
//
// THE STAGED TREE IS A BUILD ARTIFACT. `scripts/` at the repository root stays
// the single source of truth; `scripts/vscode/package.sh` copies from it and
// `editors/vscode/.gitignore` keeps the copy untracked. Nothing edits the copy,
// so the probe below is asking "was this extension packaged?", never "which
// version of the scripts is this?".
//
// Free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go) so it is
// unit-testable under plain `node --test`: `context.asAbsolutePath(p)` is
// `path.join(context.extensionPath, p)`, so the caller in src/extension.ts hands
// over the extension path and the probe plus the fallback live here, where a
// test can point them at a temporary directory.
//
// Refs: #3487 #3469 #3463 #3357

import * as fs from "node:fs";
import * as path from "node:path";

/**
 * The subdirectory a packaged extension carries its staged copy of `scripts/`
 * in. Shared with `scripts/vscode/package.sh`, which writes it.
 */
export const STAGED_ROOT_DIR = "staged";

/**
 * The staged tree's marker: the directory holding the graph documents.
 *
 * A DIRECTORY rather than a file, and the graph directory rather than a script,
 * because it is the one thing no partial stage can plausibly produce -- an
 * install cannot even begin without a graph document to read, so its absence
 * means "not packaged" with no ambiguity.
 */
const STAGED_MARKER = path.join("scripts", "install", "graph");

/**
 * Resolves the root a session's graph documents and capability scripts sit
 * under.
 *
 * A packaged extension gets the staged tree. Anything else -- the Extension
 * Development Host, which runs the extension straight out of
 * `editors/vscode/` in a checkout -- gets the repository root two levels up,
 * where the real `scripts/` lives. `checkoutRoot` overrides that computation
 * for callers that already know where the checkout is (and for tests).
 *
 * The staged tree WINS when it is present. A packaged extension may happen to
 * sit inside some unrelated directory two levels below anything; only its own
 * staged copy is known to be the tree it was built against.
 *
 * When neither exists this still returns the checkout root, and the caller
 * fails on the missing graph document with a path in the message. That is a
 * better failure than one thrown from here: the operator learns which file was
 * looked for, not merely that a resolver was unhappy.
 */
export function resolveInstallRoot(extensionPath: string, checkoutRoot?: string): string {
  const staged = path.join(extensionPath, STAGED_ROOT_DIR);
  if (isDirectory(path.join(staged, STAGED_MARKER))) {
    return staged;
  }
  return checkoutRoot ?? path.resolve(extensionPath, "..", "..");
}

/**
 * Whether a path is a directory that exists.
 *
 * Swallows the stat error on purpose: every reason a stat can fail here --
 * absent, a file, a dangling symlink, unreadable -- means the same thing to the
 * caller, which is "this is not a staged tree, use the checkout".
 */
function isDirectory(candidate: string): boolean {
  try {
    return fs.statSync(candidate).isDirectory();
  } catch {
    return false;
  }
}
