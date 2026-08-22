// The construct detail page's markup.
//
// Same three constraints as every other view-kit consumer here: no DOM, no
// inline event handlers (the webview CSP forbids them, so interactivity is
// `data-act` attributes plus one delegated listener), and every value through
// `escapeHtml`.
//
// THE ARGUMENT TABLE RENDERS THROUGH THE VALUE VIEWER'S VOCABULARY but is not
// the value viewer: an argument is a DECLARATION -- name, type, required,
// enum, description -- rather than a value, so it is a table. The viewer is
// what renders a RUN's result, and that path is `state/runResult.ts`,
// unchanged.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3752 #3747

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import type { CatalogConstruct } from "../state/constructCatalog.js";

export interface ConstructPageInput {
  construct: CatalogConstruct;
  /**
   * Whether the construct's file is reachable in this workspace. False when it
   * is not open-able from here, which is stated rather than silently offering
   * a button that does nothing.
   */
  fileInWorkspace: boolean;
  /**
   * Emit the run affordances. OFF by default, and not because running is
   * optional: it is #3753's, which wires the command and builds the RunTarget.
   * A Run button rendered before that exists would post a message nothing
   * handles -- and a click that does nothing teaches an operator the page is
   * broken. #3753 turns this on in the same change that makes it work.
   */
  offerRun?: boolean;
  /**
   * Route the run through the AUTOMATION form rather than the argument form.
   *
   * The two are different commands taking different targets, so the page has to
   * say which it means. Decided by the caller (`isAutomationRun`) rather than
   * re-derived from `kind` here, so one module owns the branch.
   */
  automationRun?: boolean;
  /**
   * Whether this host can offer to read the source from the CLUSTER instead
   * (memql#4248).
   *
   * The catalog reports a path relative to the cluster's own tree, which is
   * usually not this checkout -- so "not in this workspace" used to be the end
   * of the road. It is not: the cluster that loaded the construct also serves
   * the file, over the pack browser. Decided by the caller, which is the only
   * side that can see the workspace at all, and REQUIRED rather than optional
   * so that a new caller has to answer it instead of defaulting quietly back
   * to the dead end.
   */
  offerClusterSource: boolean;
  /** A failure this page produced, or "". */
  error: string;
}

/**
 * What the origin badge says, and what it means for opening the source.
 *
 * The four are genuinely different situations rather than four labels:
 * `core` came from the embedded tree, `bundle` from a product's DSL mounted at
 * MEMQL_DSL_PATH, and `promoted` has NO FILE AT ALL -- it lives in the
 * cluster's database, which is where a developer first meets the
 * seeded-versus-trained distinction.
 *
 * `staged` (memql#3928) is the same place as `promoted` with a different
 * audience, and the note says so in those terms: what a reader needs to know
 * about a staged construct is not where it lives but that nobody else can call
 * it yet.
 */
export function originNote(construct: CatalogConstruct): string {
  switch (construct.origin) {
    case "core":
      return "from the engine's embedded DSL tree";
    case "bundle":
      return "from the DSL bundle this cluster mounts";
    case "promoted":
      return "promoted -- it lives in this cluster's database and has no file";
    case "staged":
      return "staged -- it lives in this cluster's database, and only you can call it until it is trained";
  }
}

export function renderConstructPage(input: ConstructPageInput): string {
  const { construct } = input;

  const facts: [string, string][] = [
    ["kind", construct.kind],
    ["namespace", construct.namespace === "" ? "none" : construct.namespace],
    ["origin", `${construct.origin} -- ${originNote(construct)}`],
  ];
  if (construct.boundConcept !== "") facts.push(["bound concept", construct.boundConcept]);
  if (construct.originPath !== "") facts.push(["file", construct.originPath]);
  // EMPTY MEANS "NOT AVAILABLE", never "hashes to nothing", so it is not shown
  // as a value that could be compared against another empty one.
  if (construct.sourceHash !== "") facts.push(["source hash", construct.sourceHash]);

  const factsHtml = facts
    .map(
      ([key, value]) => `<div class="fact">
  <span class="fact-key">${escapeHtml(key)}</span>
  <span class="fact-value">${escapeHtml(value)}</span>
</div>`,
    )
    .join("");

  const error = input.error === "" ? "" : `<p class="error">${escapeHtml(input.error)}</p>`;

  return `<h1>${escapeHtml(construct.name)}</h1>
<p class="lede">${escapeHtml(construct.description === "" ? "No description." : construct.description)}</p>
${error}
<div class="facts">${factsHtml}</div>
${actionsHtml(input)}
${argsHtml(construct)}
${sourceHtml(construct)}`;
}

/**
 * The actions.
 *
 * A VIEW-ONLY KIND GETS NO RUN BUTTON -- not a disabled one. The absence is
 * the statement, exactly as in the tree.
 *
 * "Open the .memql file" is offered only when there IS a file and it is
 * reachable. For a promoted construct the source is shown below instead, and
 * saying so beats a button that opens nothing.
 *
 * "Browse rows in portal" is CONCEPTS ONLY (memql#4252) -- rows are cluster
 * state, which the portal already draws, so this hands off rather than a
 * second rows browser growing in the extension. No other kind has rows, so no
 * other kind draws the button; the absence is the statement, same as the run
 * button above.
 */
function actionsHtml(input: ConstructPageInput): string {
  const buttons: string[] = [];
  if (input.offerRun === true && input.construct.runnableKind !== undefined) {
    // An automation's button says what clicking it opens. Its run is a FORM --
    // pick a row or paste a payload, then fire a real event -- rather than the
    // immediate invocation "Run" means for the other four, and the label is the
    // only warning before a click that has consequences on a real cluster.
    const label = input.automationRun === true ? "Run automation..." : "Run";
    buttons.push(`<button class="primary" type="button" data-act="run">${label}</button>`);
    // Never for an automation: its `args` is always empty (there is no declared
    // payload schema), so the argument form has nothing to draw and the
    // automation form is where its inputs live.
    if (input.automationRun !== true && input.construct.args.length > 0) {
      buttons.push(
        `<button class="secondary" type="button" data-act="runWith">Run with arguments</button>`,
      );
    }
  }
  if (input.construct.originPath !== "" && input.fileInWorkspace) {
    buttons.push(`<button class="secondary" type="button" data-act="openFile">Open the .memql file</button>`);
  }
  // The other side of that branch, and only when there IS a file: a promoted
  // construct's source is already rendered below, so a fetch would ask the
  // cluster for something the catalog has already said does not exist.
  if (input.construct.originPath !== "" && !input.fileInWorkspace && input.offerClusterSource) {
    buttons.push(
      `<button class="secondary" type="button" data-act="viewSourceFromCluster">View source from cluster</button>`,
    );
  }
  if (input.construct.kind === "concept") {
    buttons.push(`<button class="secondary" type="button" data-act="browseRows">Browse rows in portal</button>`);
  }
  if (buttons.length === 0) return "";
  return `<div class="actions">${buttons.join("")}</div>`;
}

function argsHtml(construct: CatalogConstruct): string {
  if (construct.args.length === 0) {
    // Said rather than omitted: "this takes no arguments" and "the argument
    // list did not load" look identical as an absent section.
    return `<h2>Arguments</h2>
<p class="lede">This construct takes no arguments.</p>`;
  }
  const rows = construct.args
    .map(
      (arg) => `<div class="arg" data-required="${arg.required}">
  <span class="arg-name">${escapeHtml(arg.name)}</span>
  <span class="arg-type">${escapeHtml(arg.type)}</span>
  <span class="arg-flags">${escapeHtml(argFlags(arg))}</span>
  <span class="arg-description">${escapeHtml(arg.description ?? "")}</span>
</div>`,
    )
    .join("");
  return `<h2>Arguments</h2>
<div class="args">${rows}</div>`;
}

function argFlags(arg: CatalogConstruct["args"][number]): string {
  const flags: string[] = [];
  if (arg.required) flags.push("required");
  if (arg.autoInjected === true) {
    // Marked and still submitted: the engine stamps it and discards what was
    // sent, so hiding it would be an invisible divergence from what dispatch
    // does (memql#3333).
    flags.push("auto-injected");
  }
  if (arg.enum !== undefined && arg.enum.length > 0) {
    flags.push(`one of: ${arg.enum.join(", ")}`);
  }
  return flags.join(" - ");
}

/**
 * The source, for the one case that has nowhere else to show it.
 *
 * A file-backed construct's source is deliberately NOT shipped by the catalog
 * -- the pack browser already serves that file and the editor opens it at the
 * signature, which is a better path than a detached copy. So this section
 * appears only for a promoted construct, and says why.
 */
function sourceHtml(construct: CatalogConstruct): string {
  if (construct.source === "") return "";
  return `<h2>Source</h2>
<p class="lede">This construct has no file. What follows is what the cluster holds.</p>
<pre class="source">${escapeHtml(construct.source)}</pre>`;
}
