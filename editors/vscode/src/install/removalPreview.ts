// The uninstall preview, as a view model.
//
// `previewUninstall` (session.ts, #3469) already answers the hard questions:
// which steps have a receipt entry to act on, which artifact each one names,
// and -- the load-bearing one -- which artifacts the install merely FOUND and
// must therefore leave alone. This module does not re-answer any of them. It
// takes that answer and shapes it into the rows a renderer draws.
//
// THE VERDICT IS NOT RE-DERIVED HERE. It would be easy to read the receipt a
// second time and work out "preserved" again, and that is exactly the mistake:
// a second derivation is a second chance to disagree with the first, and the
// two things that would then disagree are the list the operator consents to and
// the flags the removal actually runs with. `preserved` arrives already
// decided; this file copies it and adds nothing.
//
// NOTHING WITH AN ARTIFACT IS DROPPED, and nothing without one is invented. A
// step that will be skipped is a step whose receipt had no entry -- there is no
// artifact, so there is nothing to tell the operator about, and a row saying so
// would be a row about a thing that does not exist on their machine.
//
// Deliberately free of `vscode` imports -- like the rest of src/install, this
// has to run as plain node from cli.ts, and being pure over a plain value is
// what lets the whole mapping be tested with no renderer and no filesystem.
//
// Refs: #3476 #3469

import * as os from "node:os";

import type { RemovalElevation, RemovalItemView } from "@znasllc-io/memql-view-kit";
import { maskHomePath } from "./secrets.js";

/**
 * The row type, re-exported so a caller of this mapping needs one import.
 *
 * It used to be REDECLARED here, because the view-kit export was still on its
 * own branch and a copy was the only way to be pure over a plain value. The
 * export has landed, so the copy is gone: two declarations of one row shape are
 * two things to keep in step, and the field they would have drifted on first is
 * `elevation`, whose whole point is that a value missing from the view model is
 * a warning the operator never sees.
 */
export type { RemovalElevation, RemovalItemView };

/**
 * The half of `PlannedStep` (session.ts, #3469) this mapping reads.
 *
 * Declared structurally, and narrowed to the fields actually consumed, so a
 * real `PlannedStep` satisfies it without a cast and no field is copied here
 * that this module has no business having an opinion about.
 */
export interface PreviewStep {
  id: string;
  description: string;
  action: "run" | "skip";
  /** Why it will be skipped. Empty for a step that will run. */
  reason: string;
  /** The artifact pre-existed the install, so it stays. */
  preserved: boolean;
  /** What will be removed, ALREADY in words -- e.g. `cluster memql`. */
  target: string;
  /**
   * What removing this artifact will ask of the operator.
   *
   * REQUIRED, not optional, and that is the point. The renderer draws nothing
   * for an absent elevation rather than defaulting it to "none", because "I was
   * not told" and "this needs no privileges" are different claims -- so an
   * optional field here would let a producer that simply forgot present a
   * sudo-taking step as one that takes nothing. `PlannedStep` has carried the
   * value off the graph since #3469; the type makes passing it on unavoidable.
   */
  elevation: RemovalElevation;
  /**
   * This artifact is NOT MemQL-only, so its removal is the operator's choice
   * (memql#3566). k3d, kubectl, mkcert and the local CA are general tools they
   * may now depend on for other work; the cluster, the checkout and the hosts
   * block exist only because MemQL was installed.
   */
  shared: boolean;
  /** What else the shared thing is good for. Empty unless `shared`. */
  sharedReason: string;
}

/** The half of `UninstallPreview` (session.ts, #3469) this mapping reads. */
export interface PreviewInput {
  /** The steps that will actually remove something. */
  removals: readonly PreviewStep[];
  /** Artifacts the install found already present. They stay. */
  preserved: readonly PreviewStep[];
}

/**
 * The reason shown when a preserved step carries none of its own.
 *
 * THIS IS A GUARD, NOT THE EXPECTED PATH. `previewUninstall` populates a
 * preserved step's reason at the source, where the receipt's pre-existence
 * verdict is actually in hand, so in practice every preserved row arrives with
 * a better sentence than this one and this constant goes unused. It survives
 * anyway because the invariant it protects belongs to THIS function: a
 * preserved row must never render bare, since a preserved artifact with no
 * stated reason reads as a removal that quietly failed -- the precise opposite
 * of what happened, which is that the installer found it already here and is
 * deliberately declining to touch it.
 *
 * Deleting it would make that property depend on a different module keeping
 * its own discipline. `reason` is typed `string`, so an empty one stays
 * representable no matter what today's producer does.
 */
const DEFAULT_PRESERVED_REASON = "it existed before the install";

/**
 * The rows the uninstall preview renders.
 *
 * REMOVALS FIRST, THEN PRESERVED, each in the order the preview already carries
 * them. Those arrays are partitioned and ordered by `previewUninstall` out of
 * the uninstall graph's own topological order -- which is NOT the reverse of
 * the install order, since mkcert has to outlive the CA it is needed to
 * uninstall. Re-sorting here would silently replace a real ordering with a
 * plausible-looking one; the renderer does not sort either. What comes back is
 * what the operator reads, top to bottom.
 */
export function removalPreviewItems(preview: PreviewInput): RemovalItemView[] {
  const items: RemovalItemView[] = [];
  for (const step of preview.removals) {
    if (isArtifactless(step)) continue;
    items.push(itemFor(step));
  }
  for (const step of preview.preserved) {
    if (isArtifactless(step)) continue;
    items.push(itemFor(step));
  }
  return items;
}

/**
 * A step with nothing behind it.
 *
 * A skip means the receipt recorded no entry for the install step this one
 * reverses, so there is no artifact on this machine and no row to draw.
 * `previewUninstall` already keeps skips out of both arrays; the check is here
 * so that the omission is a property of this mapping rather than a coincidence
 * of who calls it. A preserved step is never artifactless -- it is a step that
 * WILL run and then refuse, which is a real artifact staying put.
 */
function isArtifactless(step: PreviewStep): boolean {
  return step.action === "skip" && !step.preserved;
}

function itemFor(step: PreviewStep): RemovalItemView {
  const item: RemovalItemView = {
    id: step.id,
    label: label(step),
    kind: step.preserved ? "preserved" : "removed",
    // SET ON EVERY ROW, "none" included. The preview IS the confirmation, so
    // this list is the only moment the operator consents -- and two of the
    // seven uninstall steps stop and ask for something outside MemQL's own
    // footprint (removeHostsBlock takes root to edit the system hosts file,
    // removeLocalCA takes a trust-store prompt to withdraw a CA the browsers
    // trust). A row that omitted the value would render no marker at all, which
    // reads as "this needs nothing" -- consent obtained for a prompt the
    // operator was never shown.
    elevation: step.elevation,
  };
  if (step.preserved) {
    item.reason = step.reason.trim() !== "" ? step.reason : DEFAULT_PRESERVED_REASON;
  }
  return item;
}

/**
 * What the step is about, plus which thing it is about.
 *
 * `target` arrives ALREADY rendered ("cluster memql-local", "path
 * /home/dev/.memql/bin/k3d"), phrased off the very params the removal will be
 * given. Re-rendering it from params here would be a second phrasing to keep in
 * step with the first, and the failure would be a preview naming something
 * other than what gets removed -- so it is passed through untouched.
 *
 * It also does the work the description cannot: three of the uninstall graph's
 * seven steps remove a `binary`, and while their descriptions differ, it is the
 * target that says WHICH file is going. When the preview has no target the
 * description stands alone -- an empty "()" would advertise a value nobody has.
 */
function label(step: PreviewStep): string {
  // Descriptions are sentences in the graph document ("Delete the local k3d
  // cluster."), and a list item that ends in a full stop before its
  // parenthetical reads as two fragments. The trailing stop comes off.
  const described = step.description.trim().replace(/\.+$/, "");
  // Home masked (memql#4194, audit 32): the preview is the consent screen and
  // stays fully informative as `~/.memql/...`; the account name adds nothing.
  const target = maskHomePath(step.target.trim(), os.homedir());
  if (described !== "" && target !== "") return `${described} (${target})`;
  // A row with no words at all is unreadable, and an unreadable row is an
  // artifact the operator cannot consent to. The step id is a poor name but it
  // is always there.
  return described || target || step.id;
}

// ---------------------------------------------------------------------------
// the uninstall page's rows: names a person uses, never a param key
// ---------------------------------------------------------------------------

/**
 * What each removal is CALLED on the page, by step id.
 *
 * NOUNS, NOT THE GRAPH'S NARRATION. A step's description is a sentence about
 * doing it ("Removing the cluster and everything running inside it"), which
 * is right for the run and wrong for a list of what WILL go -- and on a kept
 * row it read as a removal that was not going to happen. The target that used
 * to follow it carried the receipt's param key ("path ~/.memql/bin/k3d",
 * "caroot ..."), which is how an internal identifier and an unmasked home
 * directory reached the page.
 *
 * A removal this table does not name falls back to its own description, so a
 * new graph step is shown in some words rather than none.
 */
const REMOVAL_NAMES: Readonly<Record<string, string>> = {
  removeCluster: "The cluster",
  removeCheckout: "Downloaded MemQL files",
  removeHostsBlock: "Local addresses",
  removeLocalCA: "Local certificate authority",
  removeToolK3d: "k3d",
  removeToolKubectl: "kubectl",
  removeToolMkcert: "mkcert",
};

/**
 * One line under a shared tool's switch: what else it may be doing for the
 * person. The graph's `sharedReason` is the long form of the same fact, and
 * the fallback for a tool this table does not name.
 */
const SHARED_NOTES: Readonly<Record<string, string>> = {
  removeLocalCA: "Other local projects may rely on it.",
  removeToolK3d: "Other local clusters may use it.",
  removeToolKubectl: "Works with any Kubernetes cluster.",
  removeToolMkcert: "Makes trusted certificates for local projects.",
};

/** The params a removal names its artifact by, in the order describeTarget reads them. */
const TARGET_KEYS = ["path", "cluster", "caroot", "hosts-file"] as const;

/** The removal step, as the page rows need it: PreviewStep plus what the preview also carries. */
export interface RowStep extends PreviewStep {
  params?: Readonly<Record<string, string>>;
  dependsOn?: readonly string[];
}

export interface RemovalRow {
  id: string;
  /** What it is: "The cluster", "Downloaded MemQL files". */
  name: string;
  /** Where it is, quietly: the cluster's name, a path with the home masked. */
  detail: string;
  /** Kept rather than removed: it was on this computer before MemQL. */
  kept: boolean;
  /** Why it is kept, in a few words. Empty on a removal. */
  reason: string;
  /** What running it will ask for: the password, or approval of a trust change. */
  asks?: "password" | "approval";
}

export interface SharedToolRow {
  id: string;
  name: string;
  note: string;
  /** The other shared removal this one cannot run without, when there is one. */
  requires?: string;
}

/** The artifact's own name or place, without the param key, home masked. */
function targetDetail(step: RowStep, home: string): string {
  if (step.id === "removeCluster") {
    const cluster = step.params?.["cluster"] ?? "";
    return cluster === "" ? "and everything running in it" : `${cluster}, and everything running in it`;
  }
  for (const key of TARGET_KEYS) {
    const value = (step.params?.[key] ?? "").trim();
    if (value !== "") return maskHomePath(value, home);
  }
  return "";
}

function nameOf(step: PreviewStep): string {
  return REMOVAL_NAMES[step.id] ?? (step.description.trim().replace(/\.+$/, "") || step.id);
}

function asksFor(elevation: RemovalElevation | undefined): RemovalRow["asks"] {
  if (elevation === "sudo") return "password";
  if (elevation === "user-trust") return "approval";
  return undefined;
}

/**
 * The page's list of what an uninstall removes and keeps: every artifact the
 * preview plans, shared tools excluded (they are the switches), removals first.
 */
export function removalRows(
  preview: { removals: readonly RowStep[]; preserved: readonly RowStep[] },
  home: string = os.homedir(),
): RemovalRow[] {
  const rows: RemovalRow[] = [];
  const add = (step: RowStep, kept: boolean): void => {
    if (step.shared || isArtifactless(step)) return;
    const row: RemovalRow = {
      id: step.id,
      name: nameOf(step),
      detail: targetDetail(step, home),
      kept,
      reason: kept ? "Was here before MemQL" : "",
    };
    const asks = asksFor(step.elevation);
    if (asks !== undefined && !kept) row.asks = asks;
    rows.push(row);
  };
  for (const step of preview.removals) add(step, false);
  for (const step of preview.preserved) add(step, true);
  return rows;
}

/**
 * The shared tools, as switches: named, one line each, and the one a tool
 * cannot be removed without.
 *
 * `requires` is read off the graph's own `dependsOn`, restricted to another
 * SHARED removal the preview offers: a dependency on the cluster is satisfied
 * by the uninstall itself, but one on the certificate authority is a choice
 * the person makes, and a tool whose removal would be skipped for want of it
 * must say so before it is chosen rather than after it did nothing.
 */
export function sharedToolRows(preview: { removals: readonly RowStep[] }): SharedToolRow[] {
  const shared = preview.removals.filter((step) => step.shared);
  const offered = new Set(shared.map((step) => step.id));
  const rows = shared.map((step): SharedToolRow => {
    const row: SharedToolRow = {
      id: step.id,
      name: nameOf(step),
      note: SHARED_NOTES[step.id] ?? step.sharedReason,
    };
    const requires = (step.dependsOn ?? []).find((id) => offered.has(id));
    if (requires !== undefined) row.requires = requires;
    return row;
  });
  // The tools first, then each dependent directly under what it depends on.
  const ordered: SharedToolRow[] = [];
  const place = (row: SharedToolRow): void => {
    if (ordered.includes(row)) return;
    ordered.push(row);
    for (const dependent of rows.filter((r) => r.requires === row.id)) place(dependent);
  };
  const byName = [...rows].sort((a, b) => rank(a.id) - rank(b.id));
  for (const row of byName) if (row.requires === undefined) place(row);
  for (const row of rows) place(row);
  return ordered;
}

/** Tools before the certificate authority, which is the one that reaches past this computer's MemQL. */
function rank(id: string): number {
  const order = ["removeToolK3d", "removeToolKubectl", "removeLocalCA", "removeToolMkcert"];
  const at = order.indexOf(id);
  return at === -1 ? order.length : at;
}
