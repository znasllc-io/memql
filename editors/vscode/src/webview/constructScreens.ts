// The construct page, built from the kit: a head, the construct's facts and
// arguments, and an action bar only when there is something to run.
//
//   spaceParticipants                                    Open source
//   Query · cognition · Built in
//   Get the people in a space.
//
//   Bound concept   v1:cognition:participant
//   Arguments                                            2
//   spaceId   string · Required
//   limit     number
//   > Details
//   ------------------------------------------------------------------
//   o Loaded on local                     Run with arguments   [ Run ]
//
// SAY IT ONCE. The kind and the namespace are the head's meta; where the
// construct came from is the meta for a built-in or bundled one and the bar's
// state for a promoted or staged one, because that is the one where it
// matters (it has no file). The file path and the source hash are details a
// reader rarely needs, so they sit behind a disclosure.
//
// ONE ACT FOR ONE INTENT. "Open source" is the only way to the source: the host
// already knows whether the file is in this workspace or has to be read from
// the cluster, so the page no longer offers two buttons for one intent. A
// promoted construct has no file at all; its source is on the page.
//
// ACTS FOLLOW THE KIND. A view-only kind gets no bar -- not a disabled Run. A
// concept's rows are one click away in the editor, and in MemQL OS.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4248 #3752 #3747

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { ORIGIN_LABELS, kindWord, type CatalogConstruct } from "../state/constructCatalog.js";
import {
  actionBar,
  button,
  disclosure,
  facts,
  head,
  notice,
  skeleton,
  subhead,
  type Act,
  type FactRow,
} from "./ui/kit.js";
import type { RegionParts } from "./ui/liveView.js";

/** Where the construct's source can be opened from, as far as this page knows. */
export type ConstructSource =
  /** The workspace lookup has not finished; the act appears when it does. */
  | "checking"
  /** The file is in this workspace. */
  | "workspace"
  /** The file is not here, and the cluster that loaded it can serve it. */
  | "cluster"
  /** There is no file: a promoted or staged construct, whose source is on the page. */
  | "none";

export interface ConstructPageInput {
  construct: CatalogConstruct;
  /** The cluster the record was read from, or "" when the opener could not say. */
  cluster: string;
  source: ConstructSource;
  /** Whether the Details disclosure is open (remembered by the host). */
  detailsOpen?: boolean;
  /** A failure this page produced, or "". */
  error?: string;
}

/** The messages the page posts. Spelled once; the panel switches on them. */
export const CONSTRUCT_ACTS = {
  run: "run",
  runWith: "runWith",
  openSource: "openSource",
  browseRows: "browseRows",
  openInOs: "openInOs",
  details: "details",
  retry: "retry",
} as const;

/**
 * Whether this page offers Run, and which kind of run.
 *
 * Decided from the construct the CATALOG described, and the same two facts
 * the run path is built from: a server-runnable kind this editor can target.
 */
function runKind(construct: CatalogConstruct): "args" | "automation" | undefined {
  if (construct.runnableKind === undefined) return undefined;
  return construct.runnableKind === "automation" ? "automation" : "args";
}

/** The head's quiet line: kind, namespace, and where it came from when that is ordinary. */
export function constructMeta(construct: CatalogConstruct): string {
  const parts = [kindWord(construct.kind), construct.namespace];
  if (construct.origin === "core" || construct.origin === "bundle") parts.push(ORIGIN_LABELS[construct.origin]);
  return parts.filter((part) => part !== "").join(" · ");
}

/** The page, whole. */
export function constructPageParts(input: ConstructPageInput): RegionParts {
  const { construct } = input;
  const asideActs: Act[] = [];
  if (input.source === "workspace" || input.source === "cluster") {
    asideActs.push({
      act: CONSTRUCT_ACTS.openSource,
      label: "Open source",
      title: input.source === "cluster" ? "Opens the file this cluster loaded, read-only" : undefined,
    });
  }
  const headHtml = head({ title: construct.name, meta: constructMeta(construct), asideActs });

  let body = "";
  if (construct.description !== "") body += `<p class="construct-desc">${escapeHtml(construct.description)}</p>`;
  if (input.error !== undefined && input.error !== "") body += notice({ tone: "error", line: input.error });

  const rows: FactRow[] = [];
  if (construct.boundConcept !== "") rows.push({ label: "Bound concept", value: construct.boundConcept, mono: true });
  if (rows.length > 0) body += facts(rows);

  body += argumentsHtml(construct);

  if (construct.kind === "concept") {
    body +=
      subhead("Rows") +
      `<div class="mq-acts">${button({ act: CONSTRUCT_ACTS.browseRows, label: "Browse rows", tone: "secondary" })}` +
      `${button({ act: CONSTRUCT_ACTS.openInOs, label: "Open in MemQL OS" })}</div>`;
  }

  // A promoted or staged construct has no file: its source is here, and this is
  // the one place it can be read.
  if (construct.source !== "") {
    body += subhead("Source") + `<pre class="construct-source"><code>${escapeHtml(construct.source)}</code></pre>`;
  }

  const details: FactRow[] = [];
  if (construct.originPath !== "") details.push({ label: "File", value: construct.originPath, mono: true });
  // EMPTY MEANS "NOT AVAILABLE", never "hashes to nothing", so it is not shown.
  if (construct.sourceHash !== "") details.push({ label: "Source hash", value: construct.sourceHash, mono: true });
  if (details.length > 0) {
    body += disclosure({
      act: CONSTRUCT_ACTS.details,
      label: "Details",
      open: input.detailsOpen === true,
      id: "construct-details",
      bodyHtml: facts(details),
    });
  }

  return { head: headHtml, body, actions: barHtml(input) };
}

function argumentsHtml(construct: CatalogConstruct): string {
  // An automation's inputs are its trigger event, which its own form builds;
  // it declares no arguments, and saying "None" about it would mislead.
  if (construct.runnableKind === "automation") return "";
  // "None" is news only about something that could take arguments: a query
  // that takes none. A concept or a spec never does, and a section saying so
  // would be a line about nothing.
  if (construct.args.length === 0) return construct.runnableKind === undefined ? "" : subhead("Arguments", "None");
  const items = construct.args
    .map((arg) => {
      const flags: string[] = [arg.type];
      if (arg.required) flags.push("Required");
      // Marked and still submitted: the engine stamps it and discards what was
      // sent, so hiding it would be an invisible divergence (memql#3333).
      if (arg.autoInjected === true) flags.push("Set by the cluster");
      if (arg.enum !== undefined && arg.enum.length > 0) flags.push(arg.enum.join(" | "));
      const description =
        arg.description === undefined || arg.description === ""
          ? ""
          : `<p class="mq-field-hint">${escapeHtml(arg.description)}</p>`;
      return (
        `<li class="construct-arg"><span class="construct-arg-head"><code class="construct-arg-name">${escapeHtml(arg.name)}</code>` +
        `<span class="construct-arg-flags mq-head-meta">${escapeHtml(flags.join(" · "))}</span></span>${description}</li>`
      );
    })
    .join("");
  return subhead("Arguments", String(construct.args.length)) + `<ul class="construct-args">${items}</ul>`;
}

/**
 * The bar: where the construct lives, and Run when it can be run.
 *
 * ABSENT for a kind with nothing to run. The bar is where state-changing acts
 * live, and browsing a concept's rows changes nothing.
 */
function barHtml(input: ConstructPageInput): string {
  const { construct } = input;
  const kind = runKind(construct);
  if (kind === undefined) return "";
  const where = input.cluster === "" ? "" : ` on ${input.cluster}`;
  const state =
    construct.origin === "promoted"
      ? `Promoted${where}`
      : construct.origin === "staged"
        ? `Staged${where}`
        : `Loaded${where}`;
  const acts: Act[] = [];
  if (kind === "automation") {
    // An automation's run is a FORM -- a trigger event to build -- so the
    // label says what the click opens.
    acts.push({ act: CONSTRUCT_ACTS.run, label: "Run...", tone: "primary" });
  } else {
    if (construct.args.length > 0) acts.push({ act: CONSTRUCT_ACTS.runWith, label: "Run with arguments" });
    acts.push({ act: CONSTRUCT_ACTS.run, label: "Run", tone: "primary" });
  }
  return actionBar({
    state,
    detail: construct.origin === "staged" ? "Only you can call it" : undefined,
    tone: "live",
    acts,
  });
}

/** While the construct is being read from the cluster. */
export function constructLoadingParts(): RegionParts {
  return { head: "", body: skeleton({ shape: "page", rows: 5, label: "Loading construct" }), actions: "" };
}

/** The read failed: say so, and offer it again. */
export function constructFailedParts(name: string, message: string): RegionParts {
  return {
    head: head({ title: name }),
    body: notice({ tone: "error", line: message, acts: [{ act: CONSTRUCT_ACTS.retry, label: "Try again" }] }),
    actions: "",
  };
}

/** Panel-local layout for the page. Layout only; the kit and the tokens own colour. */
export const CONSTRUCT_PAGE_STYLES = `
  .construct-desc { margin: -12px 0 20px; max-width: 72ch; }
  .construct-args { list-style: none; margin: 0; padding: 0; max-width: 80ch; }
  .construct-arg { padding: 5px 0; }
  .construct-arg-head { display: flex; flex-wrap: wrap; align-items: baseline; column-gap: 12px; }
  .construct-arg-name { font-weight: 600; padding: 0; background: none; }
  .construct-arg .mq-field-hint { margin-top: 2px; }
  .construct-source { margin: 0; max-width: 100%; overflow-x: auto; background: var(--memql-raised);
                      border-radius: var(--memql-radius); }
  .construct-source > code { display: block; padding: 10px 12px; white-space: pre; }
`;
