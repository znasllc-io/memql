// Running a construct that exists in no file on this machine.
//
// The run path was built around an open document: a `RunTarget` carries a
// `uri`, the orchestrator assembles a bundle from that file and its imports,
// validates it, session-defines it, and invokes. Every step assumes a buffer.
//
// A CATALOG RUN HAS NO BUFFER, and for a promoted construct there is no file
// anywhere -- it lives in the cluster's database. So this builds a target whose
// uri names the CATALOG rather than a file, and the one place that resolves a
// uri to bytes (`assembleForTarget` in extension.ts) recognises the scheme and
// assembles nothing.
//
// THAT IS THE WHOLE MECHANISM, and it is small because the orchestrator never
// reads `target.uri` itself. It reads the bundle its injected `assemble`
// returns. An empty bundle validates trivially, injects nothing, and invokes
// the definition the cluster already has -- which is exactly what running from
// a catalog means.
//
// WHAT IS DELIBERATELY REUSED UNCHANGED: `src/state/argForm.ts`,
// `src/run/preflight.ts`, the write confirmation, the supersession token, the
// Result view. In particular the NON-LOCAL MUTATION CONFIRMATION still fires,
// because it is decided from the kind and the cluster's `local` flag and has
// never had anything to do with where the source came from. Browsing a catalog
// must not become a quieter way to write to production (memql#3309).
//
// FOUR KINDS, NOT FIVE. An automation's form is decided by its TRIGGER -- which
// payload modes are offered, and which concept the row picker browses -- and
// `ListConstructs` does not carry one. A form missing the field that decides it
// would fire a real event on a real cluster with a payload nobody chose, so an
// automation renders no run affordance here. The gap is recorded rather than
// worked around; adding `trigger` to `ConstructInfo` makes it work with no
// change to this file.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3753 #3747

import type { RunTarget } from "./runnable.js";
import { usesArgForm } from "./runnable.js";
import type { CatalogConstruct } from "../state/constructCatalog.js";

/**
 * The uri scheme a catalog target carries.
 *
 * A SCHEME RATHER THAN A SENTINEL PATH, because it has to survive
 * `Uri.parse` -- the target crosses the command-argument boundary and comes
 * back as a plain object, and a path-shaped marker would be normalised on the
 * way through. A scheme VS Code has no handler for is inert: nothing will try
 * to open it, which is correct, since there is nothing to open.
 */
export const CATALOG_SCHEME = "memql-catalog";

/** Whether a target's uri names the catalog rather than a file. */
export function isCatalogUri(uri: string): boolean {
  return uri.startsWith(`${CATALOG_SCHEME}:`);
}

/**
 * A run target for a construct read from the catalog, or undefined when this
 * construct cannot be run from here.
 *
 * Undefined for three distinct reasons, all of which the page renders as the
 * ABSENCE of a run button rather than as a disabled one:
 *
 *   - the engine does not call it runnable;
 *   - it is runnable but of a kind this client cannot map (see
 *     state/constructCatalog.ts -- there is no target to build);
 *   - it is an automation, which needs a trigger the catalog does not carry.
 */
export function catalogRunTarget(construct: CatalogConstruct): RunTarget | undefined {
  const kind = construct.runnableKind;
  if (kind === undefined) return undefined;
  if (!usesArgForm(kind)) return undefined;
  return {
    uri: catalogUri(construct),
    kind,
    name: construct.name,
    args: construct.args,
  };
}

/**
 * `memql-catalog:<kind>/<name>`.
 *
 * Carries the kind and the name so a uri appearing in a log or a saved
 * configuration is legible on its own. It is not resolvable and is not meant
 * to be -- the only code that inspects it is `isCatalogUri`.
 */
export function catalogUri(construct: CatalogConstruct): string {
  return `${CATALOG_SCHEME}:${construct.kind}/${encodeURIComponent(construct.name)}`;
}

/** Whether the detail page should offer to run this construct. */
export function offersRun(construct: CatalogConstruct): boolean {
  return catalogRunTarget(construct) !== undefined;
}

/**
 * Why a runnable construct is nonetheless not offered a run here.
 *
 * Said on the page rather than left as an unexplained absence, for the one
 * case an operator will otherwise read as a bug: an automation IS runnable,
 * the tree says so, and the button is missing.
 */
export function runUnavailableReason(construct: CatalogConstruct): string {
  if (construct.runnableKind === undefined) return "";
  if (construct.runnableKind === "automation") {
    return (
      "An automation is fired by an event, and the catalog does not carry its trigger -- " +
      "so this page cannot describe the payload it would run with. Open it from its .memql file to run it."
    );
  }
  return "";
}
