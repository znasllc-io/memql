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
// FIVE KINDS. An automation's form is decided by its TRIGGER -- which payload
// modes are offered, and which concept the row picker browses -- and
// `ListConstructs` now carries one (memql#3805). Until it did, this file
// withheld the affordance rather than draw a form missing the field that
// decides it, because that form would fire a real event on a real cluster with
// a payload nobody chose.
//
// AN AUTOMATION TAKES A DIFFERENT TARGET, and that is the whole of why it is a
// separate branch here rather than a fifth entry in the arg-form set. The other
// four carry a `RunTarget` (a uri, a kind, an argument schema) and go to
// COMMAND_RUN; an automation carries an `AutomationTarget` (a uri, a name, a
// trigger, no args) and goes to COMMAND_RUN_AUTOMATION. Its `args` is always
// empty -- an automation has no declared payload schema -- so routing it
// through the argument form would render an empty form and invoke the wrong
// command.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3805 #3753 #3747

import type { AutomationTarget, RunTarget } from "./runnable.js";
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
 *   - it is an AUTOMATION, which takes an AutomationTarget instead. Ask
 *     `catalogAutomationTarget` for that one; this function returns only the
 *     targets the argument form binds.
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

/**
 * Whether the detail page should offer to run this construct at all.
 *
 * EITHER target kind counts. The page asks one question -- draw a Run control
 * or do not -- and the answer is the same for both; which command it posts is
 * `isAutomationRun`'s business, one layer in.
 */
export function offersRun(construct: CatalogConstruct): boolean {
  return catalogRunTarget(construct) !== undefined || catalogAutomationTarget(construct) !== undefined;
}

/**
 * Whether a run of this construct goes through the automation form rather than
 * the argument form.
 *
 * Exported so the page and its renderer agree on the branch instead of each
 * testing `kind === "automation"` -- the same reason `runnableKind` exists
 * rather than a set membership test on `kind`.
 */
export function isAutomationRun(construct: CatalogConstruct): boolean {
  return catalogAutomationTarget(construct) !== undefined;
}

/**
 * A run target for an AUTOMATION read from the catalog, or undefined when this
 * construct is not one that can be run that way.
 *
 * THE TRIGGER IS PASSED THROUGH, NOT DEFAULTED. An automation the cluster
 * reports no trigger for gets a target with none, and `automationFormPlan`
 * renders that as manual-run -- which is what the engine does with it. That is
 * a real, describable form, so it is offered; the case this file used to
 * withhold was a form that could not be described at all.
 */
export function catalogAutomationTarget(construct: CatalogConstruct): AutomationTarget | undefined {
  if (construct.runnableKind !== "automation") return undefined;
  const target: AutomationTarget = {
    uri: catalogUri(construct),
    name: construct.name,
  };
  if (construct.trigger !== undefined) target.trigger = { ...construct.trigger };
  return target;
}
