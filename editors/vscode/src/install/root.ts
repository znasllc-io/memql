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
 * The marker a CHECKOUT must show before its capability scripts may be run:
 * the contract library every one of them sources.
 *
 * `scripts/lib/capability.sh` rather than the graph directory, because that is
 * the thing being borrowed. `resolveScriptRoot` takes SCRIPTS from the checkout
 * and leaves the graph documents with the extension, so the graph marker would
 * be answering a question nobody asked.
 */
const SCRIPT_MARKER = path.join("scripts", "lib", "capability.sh");

/**
 * Resolves the root a session's CAPABILITY SCRIPTS are read from, which is not
 * always the root its graph documents come from.
 *
 * WHY THESE ARE TWO ANSWERS (memql#5056). `resolveInstallRoot` above prefers a
 * packaged extension's staged tree, and its reasoning holds for the case it was
 * written for: an install must be able to run BEFORE a checkout exists, so it
 * cannot read from one. The comment on that function states the assumption in
 * as many words -- the probe asks "was this extension packaged?", never "which
 * version of the scripts is this?".
 *
 * `updateCheckout` is what makes it that question. Its entire purpose is to move
 * the checkout to a commit the packaged extension has never seen, and the very
 * next step then builds that checkout. Frozen scripts driving a moved tree is
 * not a hypothetical: extension 0.3.1 asked for a `voice` node and a
 * `voice-runtime` Dockerfile stage, both retired in the commits the update had
 * just pulled, and 13 of the 66 staged files already differed from the checkout
 * they were pointed at.
 *
 * SO: a flow that builds FROM a checkout runs THAT checkout's scripts. The
 * staged tree stays the answer for bootstrap, and stops being the answer once a
 * checkout is the thing being built.
 *
 * THE GRAPH STAYS WITH THE EXTENSION, deliberately. A graph document names the
 * steps, and the extension's own UI is written against them -- a checkout
 * offering different ones would be handing the wizard a flow it cannot render.
 * Scripts are the half that must match the tree they operate on; steps are the
 * half that must match the editor driving them. That is a difference in WHAT
 * the two roots answer, not the shape drift `resolveInstallRoot` warns about.
 *
 * THE FLOOR IS THE CONTRACT LIBRARY. A checkout old enough not to carry
 * `scripts/lib/capability.sh` cannot answer the capability contract at all, so
 * it falls back to the staged tree rather than failing -- the same answer it
 * gets today, for a checkout that could not have worked either way.
 */
export function resolveScriptRoot(installRoot: string, checkoutRoot?: string): string {
  if (checkoutRoot && isFile(path.join(checkoutRoot, SCRIPT_MARKER))) {
    return checkoutRoot;
  }
  return installRoot;
}

/**
 * Whether a path is a regular file that exists.
 *
 * Swallows the stat error for the reason `isDirectory` does: absent, a
 * directory, a dangling symlink and unreadable all mean "not a usable checkout,
 * use the staged tree".
 */
function isFile(candidate: string): boolean {
  try {
    return fs.statSync(candidate).isFile();
  } catch {
    return false;
  }
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
